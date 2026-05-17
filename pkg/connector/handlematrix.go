package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/util/ffmpeg"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

const ErrGroupSMSDisabled = "Group Messages are disabled for your Dialpad account. Reach out to Dialpad Support to enable bulk SMS."

// deriveMMSCaption returns a Matrix-side caption to attach to an outbound
// MMS, or "" if Body just mirrors the filename. Compression can rename a
// file (audio .mp3 → .m4a, non-jpg images → image.jpg), so a true caption
// is a Body that matches NEITHER the original NOR the post-compression
// filename. Without checking both, a renamed file's body would surface as
// a literal "Large Voice Note.mp3" caption next to the attachment.
func deriveMMSCaption(body, originalFilename, finalFilename string) string {
	if body == "" || body == originalFilename || body == finalFilename {
		return ""
	}
	return body
}

func groupSMSBlockedMessage(details string) string {
	switch details {
	case "CONTACT_INTL_GROUP":
		return "This group includes a phone number outside the US, and Dialpad does not allow group SMS to international numbers on your plan."
	case "":
		return ErrGroupSMSDisabled
	default:
		return fmt.Sprintf("Dialpad declined to enable group SMS for this group (%s). Reach out to Dialpad Support for help.", details)
	}
}

func (da *DialpadAPI) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	da.log.Debug().
		Str("room_id", string(msg.Portal.MXID)).
		Msg("Handling Matrix message → Dialpad")

	content := msg.Content
	if content == nil {
		return nil, fmt.Errorf("missing message content")
	}

	// Enforce SMS text length limit — the framework declares MaxTextLength
	// but leaves enforcement to the connector for post-conversion safety.
	if !content.MsgType.IsMedia() && len([]rune(content.Body)) > 1600 {
		// Truncate and append a notice so the recipient knows the message was cut
		runes := []rune(content.Body)
		truncSuffix := "… [truncated]"
		maxBody := 1600 - len([]rune(truncSuffix))
		content.Body = string(runes[:maxBody]) + truncSuffix
		da.log.Warn().
			Int("original_len", len(runes)).
			Msg("Message exceeded 1600 char SMS limit, truncated")
	}

	kind, myNumber, phoneNumber, _, ok := ParsePortalID(msg.Portal.ID)
	switch {
	case !ok:
		return nil, fmt.Errorf("invalid portal ID format: %s", msg.Portal.ID)
	case kind == portalKindGroup:
		meta, _ := msg.Portal.Metadata.(*PortalMetadata)
		if meta == nil || meta.ContactKey == "" {
			return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("%s", ErrGroupSMSDisabled)).
				WithIsCertain(true).WithStatus(event.MessageStatusFail).
				WithMessage(ErrGroupSMSDisabled)
		}
		groupMyNumber := meta.MyNumber
		if groupMyNumber == "" {
			groupMyNumber = da.getMyNumber("")
		}
		return da.sendInternalMessage(ctx, msg, groupMyNumber, "")
	default:
		return da.sendInternalMessage(ctx, msg, myNumber, phoneNumber)
	}
}

// sendInternalMessage sends a message via the internal textmessage API
// (PATCH /api/textmessage/). This is the same endpoint the Dialpad web client
// uses for all outbound messages — both SMS to external numbers and messages
// to internal contacts like Dialbot.
//
// Follows the pending message pattern (like gmessages): the message stays in
// "sending" state until the echo arrives via Ably push, at which point the
// framework transitions it to "sent".
func (da *DialpadAPI) sendInternalMessage(ctx context.Context, msg *bridgev2.MatrixMessage, myNumber, phoneNumber string) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Content

	// MMS: route media messages through the two-step internal API flow
	// (POST /api/upload_file/ → PATCH /api/textmessage/ with MMS fields).
	if content.MsgType.IsMedia() {
		return da.sendMMS(ctx, msg)
	}

	// Resolve the contact_key from portal metadata
	var contactKey string
	if meta, ok := msg.Portal.Metadata.(*PortalMetadata); ok && meta != nil {
		contactKey = meta.ContactKey
	}
	if contactKey == "" {
		var err error
		contactKey, err = da.discoverContactKey(ctx, da.meta.TargetKey, phoneNumber)
		if err != nil {
			return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("could not resolve contact: %w", err)).
				WithIsCertain(true).WithStatus(event.MessageStatusFail).
				WithMessage("Could not resolve contact — try re-adding the bridge")
		}
	}

	senderKey := da.meta.TargetKey // UserProfile — who is sending
	officeKey := da.meta.OfficeKey // Office — which line the message belongs to
	if senderKey == "" || officeKey == "" {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("bridge session incomplete: sender_key=%q office_key=%q", senderKey, officeKey)).
			WithIsCertain(true).WithStatus(event.MessageStatusFail).
			WithMessage("Bridge session incomplete — try logging out and back in")
	}

	da.log.Info().
		Str("phone", phoneNumber).
		Str("my_number", myNumber).
		Str("contact_key", contactKey).
		Str("text_preview", content.Body[:min(len(content.Body), 30)]).
		Msg("Sending message via internal textmessage API")

	// Register as pending — the echo from Ably push will confirm delivery.
	// Pre-generate the feed_key so both this pending registration and the
	// eventual push echo (TextMessagePush.FeedKey) hash to the same txnID.
	// Hashing on content.Body would break for MMS without caption.
	portalID := string(msg.Portal.ID)
	feedKey := da.client.NewFeedKey()
	txnID := makeTextHash(portalID, feedKey)
	msg.AddPendingToSave(nil, txnID, da.handleRemoteEcho)

	_, err := da.client.SendInternalMessage(ctx, &dialgo.InternalMessageRequest{
		SenderKey:  senderKey,
		TargetKey:  officeKey,
		ContactKey: contactKey,
		TargetDID:  myNumber,
		Text:       content.Body,
		FeedKey:    feedKey,
	})
	if err != nil {
		msg.RemovePending(txnID)
		da.log.Err(err).Str("phone", phoneNumber).Msg("Internal textmessage API call failed")
		var apiErr *dialgo.APIError
		if errors.As(err, &apiErr) && apiErr.IsGroupSMSDisabled() {
			return nil, bridgev2.WrapErrorInStatus(err).
				WithIsCertain(true).WithStatus(event.MessageStatusFail).
				WithMessage(ErrGroupSMSDisabled)
		}
		return nil, bridgev2.WrapErrorInStatus(err).
			WithIsCertain(true).WithStatus(event.MessageStatusFail).
			WithMessage("Message failed to send")
	}

	da.log.Debug().
		Str("feed_key", feedKey).
		Str("txn_id", string(txnID)).
		Msg("Message sent, waiting for echo confirmation")

	// Message stays in "sending" state until the push echo arrives
	return &bridgev2.MatrixMessageResponse{Pending: true}, nil
}

// handleRemoteEcho is called by the framework when an outbound message echo
// arrives via the Ably push channel and matches a pending send (via text hash).
// Returns (true, ErrNoStatus): true = save the message to DB, ErrNoStatus =
// let the framework emit its own success status (same pattern as gmessages).
func (da *DialpadAPI) handleRemoteEcho(rawEvt bridgev2.RemoteMessage, dbMessage *database.Message) (bool, error) {
	return true, bridgev2.ErrNoStatus
}

// sendMMS sends a media message via the internal MMS flow:
//  1. Download the image from the Matrix homeserver
//  2. Upload to Dialpad via POST /api/upload_file/ (multipart, file_type=MMS)
//  3. Send via PATCH /api/textmessage/ with MMS metadata (same endpoint as text SMS)
//
// Echo confirmation uses the same pending-message pattern as text SMS.
func (da *DialpadAPI) sendMMS(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	// Step 1: Download from Matrix
	data, mimeType, err := da.downloadMatrixMedia(ctx, msg)
	if err != nil {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("download media: %w", err)).
			WithIsCertain(true).WithStatus(event.MessageStatusFail).
			WithMessage("Failed to download media from Matrix")
	}

	// Derive a filename from the body (Matrix uses the filename as the body for media)
	filename := msg.Content.Body
	if filename == "" {
		filename = "image.jpg"
	}
	// Remember the original — compression may rename it (audio .mp3 → .m4a,
	// non-jpg images → image.jpg). Caption detection later compares the body
	// against both the original and the post-compression name so a renamed
	// file's body doesn't get mistaken for a user-typed caption.
	originalFilename := filename

	da.log.Info().
		Str("mime", mimeType).
		Int("size", len(data)).
		Str("filename", filename).
		Msg("Downloaded media from Matrix, uploading to Dialpad")

	// Recompress oversized media before upload. Images run a separate gate
	// because carriers reject MMS over ~1-2 megapixels regardless of file
	// size — so even an efficiently-compressed full-res JPEG that fits the
	// 2 MiB wire cap can get silently dropped on delivery. Video and audio
	// only need the size check; carriers don't enforce duration/resolution
	// limits in the same way.
	if strings.HasPrefix(mimeType, "image/") && imageNeedsRecompress(data, maxMMSSize) {
		compressed, cerr := compressImageForMMS(data, maxMMSSize)
		if cerr != nil {
			return nil, bridgev2.WrapErrorInStatus(cerr).
				WithIsCertain(true).WithStatus(event.MessageStatusFail).
				WithMessage("Image too large to send via MMS even after recompression")
		}
		da.log.Info().
			Int("original_size", len(data)).
			Int("compressed_size", len(compressed)).
			Msg("Recompressed image for MMS (size or dimension cap)")
		data = compressed
		mimeType = "image/jpeg"
		if !strings.HasSuffix(strings.ToLower(filename), ".jpg") && !strings.HasSuffix(strings.ToLower(filename), ".jpeg") {
			filename = "image.jpg"
		}
	} else if len(data) > maxMMSSize {
		switch {
		case strings.HasPrefix(mimeType, "video/") && ffmpeg.Supported():
			compressed, cerr := compressVideoForMMS(ctx, ffmpegTranscoder{}, data, mimeType, maxMMSSize)
			if cerr != nil {
				return nil, bridgev2.WrapErrorInStatus(cerr).
					WithIsCertain(true).WithStatus(event.MessageStatusFail).
					WithMessage("Video too long to fit MMS size cap even at low bitrate")
			}
			da.log.Info().
				Int("original_size", len(data)).
				Int("compressed_size", len(compressed)).
				Msg("Recompressed oversized video for MMS")
			data = compressed
			mimeType = "video/mp4"
			filename = swapMediaExtension(filename, ".mp4")
		case strings.HasPrefix(mimeType, "audio/") && ffmpeg.Supported():
			compressed, cerr := compressAudioForMMS(ctx, ffmpegTranscoder{}, data, mimeType, maxMMSSize)
			if cerr != nil {
				return nil, bridgev2.WrapErrorInStatus(cerr).
					WithIsCertain(true).WithStatus(event.MessageStatusFail).
					WithMessage("Audio too long to fit MMS size cap even at minimum bitrate")
			}
			da.log.Info().
				Int("original_size", len(data)).
				Int("compressed_size", len(compressed)).
				Msg("Recompressed oversized audio for MMS")
			data = compressed
			mimeType = "audio/mp4"
			filename = swapMediaExtension(filename, ".m4a")
		}
	}

	// Step 2: Upload to Dialpad internal storage
	upload, err := da.client.UploadFile(ctx, data, filename, mimeType)
	if err != nil {
		userMsg := "Failed to upload media to Dialpad"
		// Dialpad rejects non-MMS-friendly types (PDF, docs, archives) with
		// HTTP 400 body "null". Detect and surface a clearer message so
		// users understand it's a Dialpad-side restriction, not a bug.
		var apiErr *dialgo.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 400 &&
			!strings.HasPrefix(mimeType, "image/") &&
			!strings.HasPrefix(mimeType, "video/") &&
			!strings.HasPrefix(mimeType, "audio/") {
			userMsg = fmt.Sprintf("Dialpad MMS doesn't accept %s files. Only images, video, and audio can be sent.", mimeType)
		}
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("upload to Dialpad: %w", err)).
			WithIsCertain(true).WithStatus(event.MessageStatusFail).
			WithMessage(userMsg)
	}

	da.log.Debug().
		Str("uuid", upload.UUID).
		Str("gcs", upload.GCSFilename).
		Msg("File uploaded to Dialpad storage")

	// Step 3: Send via the internal textmessage API with MMS fields.
	//
	// Resolve which Dialpad line + contact_key to send from. Groups carry the
	// my_number/contact_key on portal metadata directly (their portal IDs are
	// "group:<phones>" with no embedded my_number). DMs read my_number from
	// the portal ID and discover contact_key from the conversation list if
	// metadata is missing.
	meta, _ := msg.Portal.Metadata.(*PortalMetadata)
	kind, parsedMy, parsedPhone, _, ok := ParsePortalID(msg.Portal.ID)
	if !ok {
		return nil, fmt.Errorf("invalid portal ID format: %s", msg.Portal.ID)
	}

	var myNumber, phoneNumber, contactKey string
	switch kind {
	case portalKindGroup:
		if meta == nil || meta.ContactKey == "" {
			return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("%s", ErrGroupSMSDisabled)).
				WithIsCertain(true).WithStatus(event.MessageStatusFail).
				WithMessage(ErrGroupSMSDisabled)
		}
		contactKey = meta.ContactKey
		myNumber = meta.MyNumber
		if myNumber == "" {
			myNumber = da.getMyNumber("")
		}
	default:
		myNumber, phoneNumber = parsedMy, parsedPhone
		if meta != nil {
			contactKey = meta.ContactKey
		}
		if contactKey == "" {
			contactKey, err = da.discoverContactKey(ctx, da.meta.TargetKey, phoneNumber)
			if err != nil {
				return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("could not resolve contact: %w", err)).
					WithIsCertain(true).WithStatus(event.MessageStatusFail).
					WithMessage("Could not resolve contact — try re-adding the bridge")
			}
		}
	}

	senderKey := da.meta.TargetKey
	officeKey := da.meta.OfficeKey
	if senderKey == "" || officeKey == "" {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("bridge session incomplete")).
			WithIsCertain(true).WithStatus(event.MessageStatusFail).
			WithMessage("Bridge session incomplete — try logging out and back in")
	}

	// Text body for the MMS (optional — images can have captions).
	text := deriveMMSCaption(msg.Content.Body, originalFilename, filename)

	// Register pending echo against the feed_key Dialpad will echo back as
	// TextMessagePush.FeedKey. The previous hash(portal, filename) form
	// missed echoes for MMS without caption (echo path only hashed text,
	// which is empty for captionless MMS) → duplicate message in the room.
	feedKey := da.client.NewFeedKey()
	txnID := makeTextHash(string(msg.Portal.ID), feedKey)
	msg.AddPendingToSave(nil, txnID, da.handleRemoteEcho)

	da.log.Info().
		Str("phone", phoneNumber).
		Str("uuid", upload.UUID).
		Str("mime", mimeType).
		Msg("Sending MMS via internal textmessage API")

	_, err = da.client.SendInternalMessage(ctx, &dialgo.InternalMessageRequest{
		SenderKey:  senderKey,
		TargetKey:  officeKey,
		ContactKey: contactKey,
		TargetDID:  myNumber,
		Text:       text,
		FeedKey:    feedKey,
		MMS: &dialgo.MMSAttachment{
			ContentType: mimeType,
			Filename:    upload.Filename,
			GCSFilename: upload.GCSFilename,
			UUID:        upload.UUID,
			Bytes:       len(data),
		},
	})
	if err != nil {
		msg.RemovePending(txnID)
		return nil, bridgev2.WrapErrorInStatus(err).
			WithIsCertain(true).WithStatus(event.MessageStatusFail).
			WithMessage("MMS failed to send")
	}

	da.log.Debug().
		Str("feed_key", feedKey).
		Str("txn_id", string(txnID)).
		Msg("MMS sent, waiting for echo confirmation")

	return &bridgev2.MatrixMessageResponse{Pending: true}, nil
}
