package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// DialpadMessageEvent is the custom RemoteMessage type for SMS/text message events.
// Modeled after gmessages' MessageEvent — implements the full set of bridgev2 interfaces
// so the framework can properly handle echo matching, deduplication, and upsert.
type DialpadMessageEvent struct {
	Evt       *dialgo.SMSEvent
	da        *DialpadAPI
	portalKey networkid.PortalKey
	sender    bridgev2.EventSender
	messageID networkid.MessageID
	timestamp time.Time

	// mediaURLs collected from the SMS event (MMS attachments)
	mediaURLs []string

	// txnID is set for outbound echo messages. It is a hash of the portal + text,
	// allowing the framework's checkPendingMessage to match this echo to the
	// pending outbound send registered in HandleMatrixMessage.
	txnID networkid.TransactionID
}

// Compile-time interface checks (same pattern as gmessages MessageEvent)
var (
	_ bridgev2.RemoteMessage                  = (*DialpadMessageEvent)(nil)
	_ bridgev2.RemoteMessageWithTransactionID = (*DialpadMessageEvent)(nil)
	_ bridgev2.RemoteMessageUpsert            = (*DialpadMessageEvent)(nil)
	_ bridgev2.RemoteEventThatMayCreatePortal = (*DialpadMessageEvent)(nil)
	_ bridgev2.RemoteEventWithTimestamp       = (*DialpadMessageEvent)(nil)
)

// --- RemoteEvent interface ---

func (m *DialpadMessageEvent) GetType() bridgev2.RemoteEventType {
	return bridgev2.RemoteEventMessageUpsert
}

func (m *DialpadMessageEvent) GetPortalKey() networkid.PortalKey {
	return m.portalKey
}

func (m *DialpadMessageEvent) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.
		Int64("dialpad_msg_id", m.Evt.ID).
		Str("direction", m.Evt.Direction).
		Str("from", m.Evt.FromNumber)
}

func (m *DialpadMessageEvent) GetSender() bridgev2.EventSender {
	return m.sender
}

// --- RemoteMessage interface ---

func (m *DialpadMessageEvent) GetID() networkid.MessageID {
	return m.messageID
}

func (m *DialpadMessageEvent) ConvertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI) (*bridgev2.ConvertedMessage, error) {
	var parts []*bridgev2.ConvertedMessagePart

	// Text part
	if m.Evt.Text != "" {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    m.Evt.Text,
			},
		})
	}

	// MMS media parts. The hint comes from the push event's MMSDetails so the
	// resulting Matrix event carries the original filename + correct content
	// type instead of rendering as a generic "attachment".
	var hint *mediaHint
	if m.Evt.MMSDetails != nil {
		hint = &mediaHint{
			Filename:    m.Evt.MMSDetails.Filename,
			ContentType: m.Evt.MMSDetails.ContentType,
		}
	}
	for _, url := range m.mediaURLs {
		mediaContent, err := m.da.uploadMediaToMatrix(ctx, intent, url, hint)
		if err != nil {
			m.da.log.Warn().Err(err).Str("url", url).Msg("Failed to bridge MMS attachment (skipping)")
			continue
		}
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type:    event.EventMessage,
			Content: mediaContent,
		})
	}

	// Fallback: if no parts at all, render a placeholder naming what was
	// sent. Same shape as the backfill placeholder so the timeline matches
	// whether the message arrives via push or via FetchMessages.
	if len(parts) == 0 {
		body := "📎 Attachment unavailable (Dialpad did not serve a download URL)"
		if m.Evt.MMSDetails != nil {
			name := m.Evt.MMSDetails.Filename
			kind := m.Evt.MMSDetails.Type
			if kind == "" {
				kind = m.Evt.MMSDetails.ContentType
			}
			switch {
			case name != "" && kind != "":
				body = fmt.Sprintf("📎 %s — %s attachment unavailable (Dialpad did not serve a download URL)", name, kind)
			case name != "":
				body = fmt.Sprintf("📎 %s — attachment unavailable", name)
			case kind != "":
				body = fmt.Sprintf("📎 %s attachment unavailable (Dialpad did not serve a download URL)", kind)
			}
		}
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    body,
			},
		})
	}

	return &bridgev2.ConvertedMessage{Parts: parts}, nil
}

// --- RemoteMessageWithTransactionID interface ---
// This is the critical piece that enables echo matching. When we send a message,
// we register a pending with txnID = textHash(portalID, text). When the echo
// arrives, we compute the same hash and return it here. The framework's
// checkPendingMessage matches them.

func (m *DialpadMessageEvent) GetTransactionID() networkid.TransactionID {
	return m.txnID
}

// --- RemoteMessageUpsert interface ---
// HandleExisting is called when the framework finds an existing DB message with the
// same ID. For Dialpad, we simply skip — the message is already bridged.

func (m *DialpadMessageEvent) HandleExisting(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message) (bridgev2.UpsertResult, error) {
	zerolog.Ctx(ctx).Debug().
		Int64("dialpad_msg_id", m.Evt.ID).
		Msg("Message already exists in database, skipping duplicate")
	return bridgev2.UpsertResult{}, nil
}

// --- RemoteEventThatMayCreatePortal interface ---

func (m *DialpadMessageEvent) ShouldCreatePortal() bool {
	// Only create portals for real completed messages, not delivery statuses
	switch m.Evt.MessageStatus {
	case "failed", "undelivered", "pending":
		return false
	default:
		return true
	}
}

// --- RemoteEventWithTimestamp interface ---

func (m *DialpadMessageEvent) GetTimestamp() time.Time {
	return m.timestamp
}

// --- Helper: compute text hash for transaction ID matching ---

// makeTextHash computes a deterministic hash of portalID + message text.
// Used as a transaction ID so the framework can match outbound echoes
// to pending sends. Both the send path and the echo handler compute
// the same hash from the same inputs.
func makeTextHash(portalID string, text string) networkid.TransactionID {
	h := sha256.New()
	h.Write([]byte(portalID))
	h.Write([]byte{0x00})
	h.Write([]byte(text))
	return networkid.TransactionID("dp-" + hex.EncodeToString(h.Sum(nil))[:16])
}

// newDialpadMessageEvent constructs a DialpadMessageEvent from an SMSEvent,
// computing all derived fields (portal key, sender, timestamp, txnID).
func (da *DialpadAPI) newDialpadMessageEvent(evt *dialgo.SMSEvent) *DialpadMessageEvent {
	// Determine external number, "my number" (which Dialpad line), and direction
	var externalNumber string
	var myNumber string
	var isFromMe bool

	switch evt.Direction {
	case "inbound":
		externalNumber = evt.FromNumber
		isFromMe = false
		// For inbound: "my number" is the destination — check ToNumber, Target, or MyNumber
		if len(evt.ToNumber) > 0 {
			myNumber = evt.ToNumber[0]
		}
		if myNumber == "" && evt.Target.PhoneNumber != "" {
			myNumber = evt.Target.PhoneNumber
		}
		if myNumber == "" {
			myNumber = evt.MyNumber // Set by handleTextMessageEvent conversion
		}
	case "outbound":
		if len(evt.ToNumber) > 0 {
			externalNumber = evt.ToNumber[0]
		} else if evt.Contact.PhoneNumber != "" {
			externalNumber = evt.Contact.PhoneNumber
		}
		isFromMe = true
		// For outbound: "my number" is the sender
		myNumber = evt.FromNumber
		if myNumber == "" && evt.Target.PhoneNumber != "" {
			myNumber = evt.Target.PhoneNumber
		}
		if myNumber == "" {
			myNumber = evt.MyNumber
		}
	default:
		da.log.Warn().Str("direction", evt.Direction).Msg("Unknown SMS direction")
		return nil
	}

	// Prefer routing by contact_key. The push event includes it, and our
	// portals cache contact_key → portal_key, so this finds the right room
	// for both 1:1 and contact_group conversations. Without this, group SMS
	// echoes (where to_phone is "+ph1,+ph2") create a corrupted portal like
	// sms:+14155550101:+14155550103,+14155550102.
	var (
		portalKey          networkid.PortalKey
		portalID           string
		externalNormalized string
		myNormalized       string
		resolvedByContact  bool
	)
	if pk, ok := da.portalKeyForContact(evt.Contact.ID); ok {
		portalKey = pk
		portalID = string(pk.ID)
		resolvedByContact = true
	}

	if !resolvedByContact {
		if externalNumber == "" {
			da.log.Warn().Msg("Could not determine external phone number")
			return nil
		}

		externalNormalized = formatPhoneNumber(externalNumber)
		myNormalized = formatPhoneNumber(myNumber)

		// Fallback: if we still can't determine which of our lines, use primary
		if myNormalized == "" {
			myNormalized = da.getMyNumber("")
		}

		// Group SMS echoes carry to_phone as a comma-joined string like
		// "+ph1,+ph2". formatPhoneNumber can't normalize that, so the comma
		// survives and MakeDMPortalID would compose a corrupted DM portal
		// (sms:+my:+ph1,+ph2). The right path for groups is the contact_key
		// cache — if that missed, we have no way to route this event without
		// inventing a portal. Drop it so the caller doesn't queue a remote
		// event against a phantom room.
		if strings.ContainsRune(externalNormalized, ',') || strings.ContainsRune(myNormalized, ',') {
			da.log.Warn().
				Str("external", externalNormalized).
				Str("my_number", myNormalized).
				Str("contact_key", evt.Contact.ID).
				Msg("Dropping SMS event with comma-joined phone and no cached contact_key (group echo arrived before sync seeded the cache)")
			return nil
		}

		portalKey = networkid.PortalKey{
			ID:       MakeDMPortalID(myNormalized, externalNormalized),
			Receiver: da.login.ID,
		}
		portalID = string(portalKey.ID)
	} else {
		// We still need externalNormalized for the sender field on inbound
		// messages. For outbound the sender is IsFromMe so this is unused.
		if !isFromMe {
			externalNormalized = formatPhoneNumber(externalNumber)
		}
	}

	sender := bridgev2.EventSender{
		IsFromMe: isFromMe,
	}
	if !isFromMe {
		sender.Sender = networkid.UserID(externalNormalized)
	}

	messageID := networkid.MessageID(fmt.Sprintf("%d", evt.ID))

	// Timestamp
	var timestamp time.Time
	if evt.CreatedDate > 0 {
		if evt.CreatedDate > 1e12 {
			timestamp = time.Unix(evt.CreatedDate/1000, (evt.CreatedDate%1000)*1e6)
		} else {
			timestamp = time.Unix(evt.CreatedDate, 0)
		}
	} else {
		timestamp = time.Now()
	}

	// MMS media URLs
	mediaURLs := evt.MediaURLs
	if len(mediaURLs) == 0 && evt.MediaURL != "" {
		mediaURLs = []string{evt.MediaURL}
	}

	// Transaction ID: only set for outbound messages (echoes). The stable
	// identifier across send→echo is the client_id we sent in the request
	// body; Dialpad preserves it on outbound (orientation=internal) echoes
	// while rewriting feed_key to a server-assigned canonical key. Verified
	// via live API probe. Fall back to text hashing only if a legacy or
	// non-text_message echo path drops client_id.
	var txnID networkid.TransactionID
	if isFromMe {
		switch {
		case evt.ClientID != "":
			txnID = makeTextHash(portalID, evt.ClientID)
		case evt.Text != "":
			txnID = makeTextHash(portalID, evt.Text)
		}
	}

	return &DialpadMessageEvent{
		Evt:       evt,
		da:        da,
		portalKey: portalKey,
		sender:    sender,
		messageID: messageID,
		timestamp: timestamp,
		mediaURLs: mediaURLs,
		txnID:     txnID,
	}
}
