package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// Ensure DialpadAPI implements the backfill interface.
var _ bridgev2.BackfillingNetworkAPI = (*DialpadAPI)(nil)
var _ bridgev2.BackfillingNetworkAPIWithLimits = (*DialpadAPI)(nil)

// FetchMessages implements bridgev2.BackfillingNetworkAPI.
// Uses the internal /api/feed/ endpoint to retrieve message history.
func (da *DialpadAPI) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	da.log.Info().
		Str("portal_id", string(params.Portal.ID)).
		Bool("forward", params.Forward).
		Int("count", params.Count).
		Msg("FetchMessages called")

	portalID := string(params.Portal.ID)

	// We need the contact_key and target_key for the internal API.
	// If contact_key is missing, try to discover it by fetching conversations.
	meta, ok := params.Portal.Metadata.(*PortalMetadata)
	if !ok || meta == nil {
		meta = &PortalMetadata{}
		params.Portal.Metadata = meta
	}

	userKey := da.meta.TargetKey
	officeKey := da.meta.OfficeKey
	if userKey == "" || officeKey == "" {
		da.log.Debug().
			Str("target_key", userKey).
			Str("office_key", officeKey).
			Msg("Missing entity keys, cannot backfill")
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}

	if meta.ContactKey == "" {
		// Groups always carry ContactKey from CreateGroup; if it's missing here, the
		// portal predates the group create path and we have no way to recover.
		if IsGroupPortalID(params.Portal.ID) {
			da.log.Debug().Str("portal_id", portalID).Msg("Group portal has no contact_key; cannot backfill")
			return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
		}
		_, _, externalNumber, _, ok := ParsePortalID(params.Portal.ID)
		if !ok {
			da.log.Debug().Str("portal_id", portalID).Msg("Unparseable portal ID; cannot backfill")
			return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
		}
		contactKey, err := da.discoverContactKey(ctx, userKey, externalNumber)
		if err != nil {
			da.log.Debug().Err(err).Str("phone", externalNumber).Msg("Failed to discover contact_key")
			return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
		}
		meta.ContactKey = contactKey
		if saveErr := params.Portal.Save(ctx); saveErr != nil {
			da.log.Warn().Err(saveErr).Msg("Failed to persist discovered contact_key")
		}
	}

	// /api/feed/ scope: each conversation lives under UserProfile (personal-line
	// chats) OR Office (office-line chats). /api/contact/ reports target_key
	// unreliably (often UserProfile for both), so try the cached target first
	// and fall back to the other scope if it returns empty.
	primaryTarget := meta.TargetKey
	if primaryTarget == "" {
		primaryTarget = userKey
	}
	fallbackTarget := officeKey
	if primaryTarget == officeKey {
		fallbackTarget = userKey
	}

	limit := params.Count
	if limit <= 0 {
		limit = 25
	}

	da.log.Debug().
		Str("portal_id", portalID).
		Str("contact_key", meta.ContactKey).
		Str("feed_target", primaryTarget).
		Int("limit", limit).
		Msg("Fetching message history")

	feedMessages, nextCursor, hasMore, err := da.getMessageHistoryPage(ctx, meta.ContactKey, primaryTarget, limit, params)
	if err != nil {
		return nil, fmt.Errorf("fetch message history: %w", err)
	}

	if len(feedMessages) == 0 && shouldRetryFallbackFeedTarget(params) && fallbackTarget != "" && fallbackTarget != primaryTarget {
		da.log.Debug().
			Str("portal_id", portalID).
			Str("fallback_target", fallbackTarget).
			Msg("Primary target returned empty, retrying with fallback target")
		fallbackMessages, fallbackCursor, fallbackHasMore, ferr := da.getMessageHistoryPage(ctx, meta.ContactKey, fallbackTarget, limit, params)
		if ferr != nil {
			da.log.Warn().Err(ferr).Msg("Fallback feed query failed")
		} else if len(fallbackMessages) > 0 {
			feedMessages = fallbackMessages
			nextCursor = fallbackCursor
			hasMore = fallbackHasMore
			meta.TargetKey = fallbackTarget
			if saveErr := params.Portal.Save(ctx); saveErr != nil {
				da.log.Warn().Err(saveErr).Msg("Failed to persist corrected target_key")
			}
		}
	}

	da.log.Debug().
		Str("portal_id", portalID).
		Int("returned", len(feedMessages)).
		Msg("Feed API returned messages")

	// Convert FeedMessage → BackfillMessage (oldest first)
	messages := make([]*bridgev2.BackfillMessage, 0, len(feedMessages))

	// The API returns newest-first, so reverse for bridgev2 (oldest-first requirement)
	for i := len(feedMessages) - 1; i >= 0; i-- {
		fm := &feedMessages[i]
		bm, err := da.convertFeedMessage(ctx, params.Portal, fm)
		if err != nil {
			da.log.Warn().Err(err).Int64("msg_id", fm.ID).Msg("Failed to convert feed message, skipping")
			continue
		}
		messages = append(messages, bm)
	}

	return &bridgev2.FetchMessagesResponse{
		Messages: messages,
		Cursor:   nextCursor,
		HasMore:  hasMore,
		Forward:  params.Forward,
		MarkRead: true, // SMS backfill is historical — don't generate unread notifications
	}, nil
}

// GetBackfillMaxBatchCount maps the bridgev2 queue overrides in config.yaml to
// Dialpad portal types. Without this hook, bridgev2 only sees the global
// max_batches value and ignores the dm/group_dm overrides.
func (da *DialpadAPI) GetBackfillMaxBatchCount(_ context.Context, portal *bridgev2.Portal, _ *database.BackfillTask) int {
	if IsGroupPortalID(portal.ID) {
		return da.connector.br.Config.Backfill.Queue.GetOverride("group_dm")
	}
	return da.connector.br.Config.Backfill.Queue.GetOverride("dm")
}

func (da *DialpadAPI) getMessageHistoryPage(
	ctx context.Context,
	contactKey string,
	targetKey string,
	limit int,
	params bridgev2.FetchMessagesParams,
) ([]dialgo.FeedMessage, networkid.PaginationCursor, bool, error) {
	if params.Forward || params.Cursor != "" || params.AnchorMessage == nil {
		page, err := da.client.GetMessageHistoryPage(ctx, contactKey, targetKey, limit, string(params.Cursor))
		if err != nil {
			return nil, "", false, err
		}
		if params.Forward && params.AnchorMessage != nil {
			page.Messages = feedMessagesAfterAnchor(page.Messages, params.AnchorMessage)
			if len(page.Messages) == 0 {
				return nil, "", false, nil
			}
		} else if params.AnchorMessage != nil {
			page.Messages = feedMessagesBeforeAnchor(page.Messages, params.AnchorMessage)
			if len(page.Messages) == 0 {
				return nil, "", false, nil
			}
		}
		messages, cursor, hasMore := da.feedHistoryPageResult(page, string(params.Cursor))
		return messages, cursor, hasMore, nil
	}
	return da.getMessageHistoryPageBeforeAnchor(ctx, contactKey, targetKey, limit, params.AnchorMessage)
}

func (da *DialpadAPI) getMessageHistoryPageBeforeAnchor(
	ctx context.Context,
	contactKey string,
	targetKey string,
	limit int,
	anchor *database.Message,
) ([]dialgo.FeedMessage, networkid.PaginationCursor, bool, error) {
	cursor := ""
	seenCursors := map[string]bool{}
	for {
		page, err := da.client.GetMessageHistoryPage(ctx, contactKey, targetKey, limit, cursor)
		if err != nil {
			return nil, "", false, err
		}
		if len(page.Messages) == 0 {
			return nil, "", false, nil
		}
		for i := range page.Messages {
			if !feedMessageMatchesAnchor(&page.Messages[i], anchor.ID) {
				continue
			}
			olderMessages := feedMessagesBeforeAnchor(page.Messages[i+1:], anchor)
			if len(olderMessages) > 0 {
				messages, nextCursor, hasMore := da.feedHistoryPageResult(&dialgo.FeedMessagePage{
					Messages: olderMessages,
					Cursor:   page.Cursor,
				}, cursor)
				return messages, nextCursor, hasMore, nil
			}
			if page.Cursor == "" {
				return nil, "", false, nil
			}
			nextPage, err := da.client.GetMessageHistoryPage(ctx, contactKey, targetKey, limit, page.Cursor)
			if err != nil {
				return nil, "", false, err
			}
			nextPage.Messages = feedMessagesBeforeAnchor(nextPage.Messages, anchor)
			if len(nextPage.Messages) == 0 {
				return nil, "", false, nil
			}
			messages, nextCursor, hasMore := da.feedHistoryPageResult(nextPage, page.Cursor)
			return messages, nextCursor, hasMore, nil
		}
		olderMessages := feedMessagesBeforeAnchor(page.Messages, anchor)
		if len(olderMessages) > 0 {
			da.log.Debug().
				Str("anchor_message_id", string(anchor.ID)).
				Time("anchor_timestamp", anchor.Timestamp).
				Msg("Could not find anchor message ID in Dialpad feed page, using anchor timestamp cutoff")
			messages, nextCursor, hasMore := da.feedHistoryPageResult(&dialgo.FeedMessagePage{
				Messages: olderMessages,
				Cursor:   page.Cursor,
			}, cursor)
			return messages, nextCursor, hasMore, nil
		}
		if page.Cursor == "" || seenCursors[page.Cursor] {
			break
		}
		seenCursors[page.Cursor] = true
		cursor = page.Cursor
	}
	da.log.Debug().
		Str("anchor_message_id", string(anchor.ID)).
		Time("anchor_timestamp", anchor.Timestamp).
		Msg("Could not find older Dialpad feed messages before anchor")
	return nil, "", false, nil
}

func feedMessageMatchesAnchor(fm *dialgo.FeedMessage, anchorID networkid.MessageID) bool {
	id := strconv.FormatInt(fm.ID, 10)
	anchor := string(anchorID)
	return anchor == id || anchor == "vm-"+id || anchor == "call-"+id || strings.HasPrefix(anchor, "call-"+id+"-")
}

func shouldRetryFallbackFeedTarget(params bridgev2.FetchMessagesParams) bool {
	return params.Cursor == "" && params.AnchorMessage == nil
}

func (da *DialpadAPI) feedHistoryPageResult(page *dialgo.FeedMessagePage, requestCursor string) ([]dialgo.FeedMessage, networkid.PaginationCursor, bool) {
	if page == nil || len(page.Messages) == 0 || page.Cursor == "" {
		if page != nil {
			return page.Messages, "", false
		}
		return nil, "", false
	}
	if page.Cursor == requestCursor {
		da.log.Debug().
			Str("cursor", page.Cursor).
			Msg("Dialpad feed cursor did not advance, stopping backfill pagination")
		return page.Messages, "", false
	}
	return page.Messages, networkid.PaginationCursor(page.Cursor), true
}

func feedMessagesBeforeAnchor(messages []dialgo.FeedMessage, anchor *database.Message) []dialgo.FeedMessage {
	if anchor == nil || anchor.Timestamp.IsZero() {
		return nil
	}
	olderMessages := make([]dialgo.FeedMessage, 0, len(messages))
	for i := range messages {
		if feedMessageMatchesAnchor(&messages[i], anchor.ID) {
			continue
		}
		msgTime := feedMessageTimestamp(&messages[i])
		if !msgTime.IsZero() && msgTime.Before(anchor.Timestamp) {
			olderMessages = append(olderMessages, messages[i])
		}
	}
	return olderMessages
}

func feedMessagesAfterAnchor(messages []dialgo.FeedMessage, anchor *database.Message) []dialgo.FeedMessage {
	if anchor == nil || anchor.Timestamp.IsZero() {
		return messages
	}
	newerMessages := make([]dialgo.FeedMessage, 0, len(messages))
	for i := range messages {
		if feedMessageMatchesAnchor(&messages[i], anchor.ID) {
			continue
		}
		msgTime := feedMessageTimestamp(&messages[i])
		if !msgTime.IsZero() && !msgTime.Before(anchor.Timestamp) {
			newerMessages = append(newerMessages, messages[i])
		}
	}
	return newerMessages
}

func feedMessageTimestamp(fm *dialgo.FeedMessage) time.Time {
	switch {
	case fm.FeedType == "Call" && fm.DateStarted > 0:
		return time.UnixMilli(fm.DateStarted)
	case fm.FeedType == "Call" && fm.FeedDate > 0:
		return time.UnixMilli(fm.FeedDate)
	case fm.Date > 0:
		return time.UnixMilli(fm.Date)
	case fm.FeedDate > 0:
		return time.UnixMilli(fm.FeedDate)
	default:
		return time.Time{}
	}
}

// convertFeedMessage converts a single internal API FeedMessage to a bridgev2.BackfillMessage.
// Handles both SMS messages and Call entries from the feed.
func (da *DialpadAPI) convertFeedMessage(ctx context.Context, portal *bridgev2.Portal, fm *dialgo.FeedMessage) (*bridgev2.BackfillMessage, error) {
	// Call entries use completely different fields than SMS
	if fm.FeedType == "Call" {
		return da.convertFeedCallEntry(ctx, portal, fm)
	}

	// SMS entry
	isFromMe := fm.Orientation == "internal"

	da.log.Debug().
		Int64("msg_id", fm.ID).
		Str("orientation", fm.Orientation).
		Str("from_phone", fm.FromPhone).
		Str("to_phone", fm.ToPhone).
		Str("feed_type", fm.FeedType).
		Str("delivery_method", fm.DeliveryMethod).
		Bool("is_from_me", isFromMe).
		Msg("Converting feed message")

	sender := bridgev2.EventSender{
		IsFromMe: isFromMe,
	}
	if !isFromMe {
		senderPhone := fm.FromPhone
		if senderPhone == "" {
			// Fallback: extract external number from the portal ID. Only DMs
			// have an "external" — for groups the sender must come from
			// fm.FromPhone (and we leave senderPhone empty if it didn't).
			if _, _, externalNumber, _, ok := ParsePortalID(portal.ID); ok {
				senderPhone = externalNumber
			}
			da.log.Warn().Int64("msg_id", fm.ID).Str("fallback_phone", senderPhone).Msg("from_phone empty, using portal external number as sender")
		}
		sender.Sender = networkid.UserID(senderPhone)
	}

	var parts []*bridgev2.ConvertedMessagePart

	if fm.Text != "" {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    fm.Text,
			},
		})
	}

	// Handle MMS media if present. Two failure modes both end up rendering a
	// placeholder Matrix message so the timeline matches Dialpad's reality —
	// dropping the message entirely was the wrong behavior.
	//
	//   1. mms_url present but Dialpad's resize service returns 404. Observed
	//      in real testing today: outbound MMS where Dialpad's own feed shows
	//      delivery_status=pending and the served URL hard-404s with
	//      "[ResizeUtils] Failed to open image" for every auth combo. Not a
	//      bridge bug — Dialpad-side state.
	//   2. delivery_method=mms but no mms_url. Observed for outbound audio
	//      MMS and some group MMS sends — Dialpad's feed simply omits the
	//      download URL even though the recipient did get the message.
	isMMS := fm.DeliveryMethod == "mms" || fm.MMSURL != "" || fm.MMSDetails != nil

	if len(parts) == 0 && !isMMS {
		// Truly empty entry (no text, not an MMS). Skip it.
		return nil, fmt.Errorf("empty message")
	}

	if fm.MMSURL != "" {
		intent, ok := portal.GetIntentFor(ctx, sender, da.login, bridgev2.RemoteEventBackfill)
		if !ok {
			da.log.Warn().Int64("msg_id", fm.ID).Msg("Failed to get intent for MMS backfill upload")
		} else {
			var hint *mediaHint
			if fm.MMSDetails != nil {
				hint = &mediaHint{
					Filename:    fm.MMSDetails.Filename,
					ContentType: fm.MMSDetails.ContentType,
				}
			}
			mediaContent, err := da.uploadMediaToMatrix(ctx, intent, fm.MMSURL, hint)
			if err != nil {
				da.log.Warn().Err(err).Str("url", fm.MMSURL).Int64("msg_id", fm.ID).Msg("MMS attachment unfetchable from Dialpad — rendering placeholder")
			} else {
				parts = append(parts, &bridgev2.ConvertedMessagePart{
					Type:    event.EventMessage,
					Content: mediaContent,
				})
			}
		}
	}

	// If the entry was an MMS but we didn't end up with a media part (either
	// no URL in the feed, or the URL 404'd), render a placeholder naming the
	// file so the recipient/timeline still shows that an attachment was sent.
	if isMMS && len(parts) == 0 {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    placeholderForMissingMMS(fm),
			},
		})
	}

	ts := time.UnixMilli(fm.Date)

	return &bridgev2.BackfillMessage{
		ConvertedMessage: &bridgev2.ConvertedMessage{Parts: parts},
		Sender:           sender,
		ID:               networkid.MessageID(fmt.Sprintf("%d", fm.ID)),
		Timestamp:        ts,
	}, nil
}

// convertFeedCallEntry converts a call entry from the feed API into a backfill message.
// Categories from HAR evidence: "incoming" (answered), "missed", "outgoing", "cancelled"
// For voicemail entries with a recording, this downloads the MP3 and bridges it
// as a voice note (m.audio + MSC3245Voice) instead of the transcription text.
func (da *DialpadAPI) convertFeedCallEntry(ctx context.Context, portal *bridgev2.Portal, fm *dialgo.FeedMessage) (*bridgev2.BackfillMessage, error) {
	// Calls are always from the external party's perspective for room placement
	isFromMe := fm.Direction == "outbound"
	sender := bridgev2.EventSender{
		IsFromMe: isFromMe,
	}
	if !isFromMe && fm.ExternalEndpoint != "" {
		sender.Sender = networkid.UserID(fm.ExternalEndpoint)
	}

	// Use date_started for timestamp (more accurate than feed_date for calls)
	var ts time.Time
	if fm.DateStarted > 0 {
		ts = time.UnixMilli(fm.DateStarted)
	} else if fm.FeedDate > 0 {
		ts = time.UnixMilli(fm.FeedDate)
	} else {
		ts = time.UnixMilli(fm.Date)
	}

	// Voicemail with recording → bridge as voice note (audio), not transcription text
	if fm.Category == "voicemail" && fm.Recording != nil && fm.Recording.RecordingURL != "" {
		return da.convertVoicemailBackfill(ctx, portal, fm, sender, ts)
	}

	// All other call types → text notice
	var text string
	switch fm.Category {
	case "missed":
		text = "📞 Missed call"
	case "voicemail":
		// No recording URL available — fallback notice
		text = "📞 Voicemail"
	case "incoming":
		if fm.DurationConnected > 0 {
			text = fmt.Sprintf("📞 Incoming call (%ds)", fm.DurationConnected)
		} else {
			text = "📞 Incoming call"
		}
	case "outgoing":
		if fm.DurationConnected > 0 {
			text = fmt.Sprintf("📞 Outgoing call (%ds)", fm.DurationConnected)
		} else {
			text = "📞 Outgoing call"
		}
	case "cancelled":
		text = "📞 Cancelled call"
	default:
		if fm.Duration > 0 {
			text = fmt.Sprintf("📞 Call (%ds)", fm.Duration)
		} else {
			text = "📞 Call"
		}
	}

	return &bridgev2.BackfillMessage{
		ConvertedMessage: &bridgev2.ConvertedMessage{
			Parts: []*bridgev2.ConvertedMessagePart{{
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType: event.MsgNotice,
					Body:    text,
				},
			}},
		},
		Sender:    sender,
		ID:        networkid.MessageID(fmt.Sprintf("call-%d", fm.ID)),
		Timestamp: ts,
	}, nil
}

// convertVoicemailBackfill downloads the voicemail MP3, uploads it to Matrix,
// and creates a proper voice note message (m.audio + MSC3245Voice + MSC1767Audio).
// This mirrors the real-time voicemail handler in handle_dialpad.go.
func (da *DialpadAPI) convertVoicemailBackfill(ctx context.Context, portal *bridgev2.Portal, fm *dialgo.FeedMessage, sender bridgev2.EventSender, ts time.Time) (*bridgev2.BackfillMessage, error) {
	recording := fm.Recording

	// Download the voicemail MP3 from Dialpad
	audioData, err := da.client.DownloadMedia(ctx, recording.RecordingURL)
	if err != nil {
		return nil, fmt.Errorf("download voicemail recording: %w", err)
	}

	da.log.Info().
		Int64("call_id", fm.ID).
		Int("size_bytes", len(audioData)).
		Int("duration_sec", recording.Duration).
		Msg("Downloaded voicemail for backfill")

	// Get intent for uploading media to Matrix (same pattern as gmessages backfill)
	intent, ok := portal.GetIntentFor(ctx, sender, da.login, bridgev2.RemoteEventBackfill)
	if !ok {
		return nil, fmt.Errorf("failed to get intent for voicemail upload")
	}

	// Detect MIME type
	mimeType := http.DetectContentType(audioData)
	if mimeType == "application/octet-stream" {
		mimeType = "audio/mpeg"
	}

	// Upload to Matrix
	uri, encFile, err := intent.UploadMedia(ctx, "", audioData, "", mimeType)
	if err != nil {
		return nil, fmt.Errorf("upload voicemail to Matrix: %w", err)
	}

	durationMs := recording.Duration * 1000
	captionText := "📞 Voicemail"
	fileNameText := "voicemail.mp3"

	return &bridgev2.BackfillMessage{
		ConvertedMessage: &bridgev2.ConvertedMessage{
			Parts: []*bridgev2.ConvertedMessagePart{{
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType:  event.MsgAudio,
					Body:     captionText,
					FileName: fileNameText,
					URL:      uri,
					File:     encFile,
					Info: &event.FileInfo{
						MimeType: mimeType,
						Size:     len(audioData),
						Duration: durationMs,
					},
					MSC3245Voice: &event.MSC3245Voice{},
					MSC1767Audio: &event.MSC1767Audio{
						Duration: durationMs,
					},
				},
			}},
		},
		Sender:    sender,
		ID:        networkid.MessageID(fmt.Sprintf("vm-%d", fm.ID)),
		Timestamp: ts,
	}, nil
}

// discoverContactKey fetches the conversation list and finds the contact_key
// placeholderForMissingMMS returns the human-readable Matrix notice we
// render when an MMS feed entry can't be turned into a real attachment.
// Includes the filename and type when MMSDetails is populated so the user
// at least sees what was sent.
func placeholderForMissingMMS(fm *dialgo.FeedMessage) string {
	if fm.MMSDetails != nil {
		name := fm.MMSDetails.Filename
		kind := fm.MMSDetails.Type
		if kind == "" && fm.MMSDetails.ContentType != "" {
			kind = fm.MMSDetails.ContentType
		}
		switch {
		case name != "" && kind != "":
			return fmt.Sprintf("📎 %s — %s attachment unavailable (Dialpad did not serve a download URL)", name, kind)
		case name != "":
			return fmt.Sprintf("📎 %s — attachment unavailable", name)
		case kind != "":
			return fmt.Sprintf("📎 %s attachment unavailable (Dialpad did not serve a download URL)", kind)
		}
	}
	return "📎 Attachment unavailable (Dialpad did not serve a download URL)"
}

// for a conversation with the given phone number. targetKey must be the
// user's UserProfile key — /api/contact/ is scoped per-user, and returns
// conversations from all of the user's lines (primary + secondary/campaign).
func (da *DialpadAPI) discoverContactKey(ctx context.Context, targetKey, phoneNumber string) (string, error) {
	conversations, err := da.client.GetConversations(ctx, targetKey, 100)
	if err != nil {
		return "", fmt.Errorf("get conversations: %w", err)
	}

	normalizedTarget := formatPhoneNumber(phoneNumber)

	for _, conv := range conversations {
		// Structured phone fields are the authoritative match. The web client
		// returns real contact names in DisplayName, so a substring check on
		// that was finding nothing for saved contacts.
		if formatPhoneNumber(conv.PrimaryPhone) == normalizedTarget {
			return conv.ContactKey, nil
		}
		if formatPhoneNumber(conv.DialString) == normalizedTarget {
			return conv.ContactKey, nil
		}
		for _, p := range conv.Phones {
			if formatPhoneNumber(p) == normalizedTarget {
				return conv.ContactKey, nil
			}
		}
		// Fallback: display name actually contains the raw number (unsaved contacts)
		if strings.Contains(conv.DisplayName, phoneNumber) {
			return conv.ContactKey, nil
		}
	}

	// No existing conversation — ask Dialpad to get-or-create one.
	if lookup, err := da.client.LookupContactByPhone(ctx, normalizedTarget, da.meta.OfficeKey); err == nil && lookup.ContactKey != "" {
		return lookup.ContactKey, nil
	}

	return "", fmt.Errorf("no conversation found for phone %s", phoneNumber)
}
