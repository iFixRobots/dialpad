package connector

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// handleSMSEvent converts an incoming Dialpad SMS event into a Matrix message.
//
// Uses DialpadMessageEvent (custom RemoteMessage) instead of simplevent.Message
// to support the full bridgev2 lifecycle: echo matching via TransactionID,
// idempotent upsert for deduplication, and conditional portal creation.
//
// Outbound messages are NOT filtered here — they are dispatched to the framework
// with IsFromMe=true and a txnID. The framework's checkPendingMessage matches
// the echo to the pending outbound send registered in HandleMatrixMessage.
func (da *DialpadAPI) handleSMSEvent(evt *dialgo.SMSEvent) {
	da.log.Debug().
		Int64("id", evt.ID).
		Str("direction", evt.Direction).
		Str("from", evt.FromNumber).
		Str("status", evt.MessageStatus).
		Msg("Processing Dialpad SMS event")

	// Handle delivery status updates
	switch evt.MessageStatus {
	case "failed", "undelivered":
		reason := evt.MessageDeliveryResult
		if reason == "" {
			reason = evt.MessageStatus
		}
		da.bridgeDeliveryFailure(evt, reason)
		return
	case "pending":
		da.log.Debug().Str("status", evt.MessageStatus).Msg("Skipping pending SMS event")
		return
	}

	// Skip empty messages (delivery receipts without text)
	if evt.Text == "" && !evt.MMS {
		da.log.Debug().Msg("Skipping SMS event with no text and no media")
		return
	}

	// Build the custom event — this handles portal key, sender, timestamp,
	// txnID (for outbound echo matching), and media URL collection.
	msgEvt := da.newDialpadMessageEvent(evt)
	if msgEvt == nil {
		// newDialpadMessageEvent logs the reason (unknown direction, no phone, etc.)
		return
	}

	da.login.QueueRemoteEvent(msgEvt)
}

// callEventMaxAge prevents bridging stale call events from before the bridge connected.
const callEventMaxAge = 15 * time.Minute

// portalKeyForCall resolves which portal a call event belongs to. Prefers an
// existing portal with the same contact_key (so a call lands in the room
// already established for SMS with this contact, even when the call rings
// the user's primary line but the SMS conversation is on the office line).
// Falls back to phone-based composition for genuinely new conversations.
//
// Returns the portal key plus the my_number that should be used as the
// "my" side of the portal ID for any downstream logic that still needs it.
func (da *DialpadAPI) portalKeyForCall(evt *dialgo.CallEvent, callerNormalized string) networkid.PortalKey {
	if pk, ok := da.portalKeyForContact(evt.GetContactKey()); ok {
		return pk
	}
	myNumber := da.getMyNumber(formatPhoneNumber(evt.GetMyNumber()))
	return networkid.PortalKey{
		ID:       MakeDMPortalID(myNumber, callerNormalized),
		Receiver: da.login.ID,
	}
}

// handleCallEvent converts an incoming Dialpad call event into a Matrix call notice.
// Follows the exact same pattern as mautrix-whatsapp's handleWACallStart.
func (da *DialpadAPI) handleCallEvent(evt *dialgo.CallEvent) {
	if !da.connector.Config.CallStartNotices {
		return
	}

	da.log.Debug().
		Int64("call_uuid", evt.CallUUID).
		Str("state", evt.State).
		Str("direction", evt.GetDirection()).
		Str("contact", evt.GetExternalNumber()).
		Msg("Processing Dialpad call event")

	// Suppress secondary-leg hangup events. Dialpad fans one inbound call
	// into a primary leg (carries missed/voicemail flags) and a secondary leg
	// per ringing device (sends ringing + a redundant hangup). The secondary
	// leg's ringing is still useful — it's typically the only "incoming
	// call" signal we see — but its hangup duplicates the primary's
	// missed-call notice. Keep secondary ringing, drop everything else from
	// secondary legs.
	if evt.Call != nil && evt.Call.IsSecondary == 1 && evt.State != dialgo.CallStateRinging {
		da.log.Debug().
			Int64("call_uuid", evt.CallUUID).
			Str("state", evt.State).
			Msg("Skipping secondary-leg non-ringing event (primary leg owns the lifecycle)")
		return
	}

	// Bridge hangup (missed/completed), ringing, and voicemail states.
	// "connected" means the call was answered and is in progress — not useful as a notice.
	switch evt.State {
	case dialgo.CallStateHangup:
		// A hangup event means the call ended. If missed=1, it's a missed call.
		// Voicemail recordings arrive inline on hangup events with VM=1 — see
		// the voicemail emission below this switch.
	case dialgo.CallStateRinging:
		// Inbound ringing — bridge as incoming call
	case dialgo.CallStateVoicemail:
		// Legacy path: some Dialpad configurations may emit a separate
		// "voicemail" state event mid-recording, in which case we poll
		// /api/call/{id} for the URL. Current production sends the recording
		// inline on the hangup event (handled below); keep this path as
		// fallback only.
		da.log.Info().
			Int64("call_uuid", evt.CallUUID).
			Msg("Voicemail state detected, polling for recording")
		go da.handleVoicemailCheck(evt)
		return
	default:
		da.log.Debug().Str("state", evt.State).Msg("Skipping non-notice call state")
		return
	}

	// Voicemail piggybacks on the hangup event. When VM=1, Dialpad embeds the
	// recording (URL + transcription + duration) directly in evt.Call.Recording.
	// Emit the audio as a second Matrix message after the "Missed call" notice.
	if evt.State == dialgo.CallStateHangup && evt.VM == 1 &&
		evt.Call != nil && evt.Call.Recording != nil && evt.Call.Recording.RecordingURL != "" {
		go da.emitVoicemailRecording(evt, evt.Call.Recording)
	}

	// Determine caller info
	callerNumber := evt.GetExternalNumber()
	if callerNumber == "" {
		da.log.Warn().Int64("call_uuid", evt.CallUUID).Msg("Call event has no contact phone number, skipping")
		return
	}

	// Clean up formatted numbers like "(415) 555-0102" → digits only
	callerNumber = cleanPhoneNumber(callerNumber)
	if callerNumber == "" {
		// Ably events often have contact_name="Gerardo Rodriguez" but no
		// caller_id phone number. For ringing we poll the active calls API.
		// For hangup we can't poll (call is over), so skip silently —
		// the ringing event already created the room entry.
		if evt.State == dialgo.CallStateRinging {
			da.log.Info().
				Str("raw", evt.GetExternalNumber()).
				Str("contact_name", evt.ContactName).
				Msg("Ringing event has name but no phone — polling active calls API")
			go da.handleActiveCallCheck()
			return
		}
		da.log.Debug().
			Str("raw", evt.GetExternalNumber()).
			Str("state", evt.State).
			Msg("Call event has name-only contact, skipping (ringing event already handled)")
		return
	}

	timestamp := time.Now()
	if evt.Call != nil && evt.Call.DateStarted > 0 {
		ts := evt.Call.DateStarted
		if ts > 1e12 {
			timestamp = time.Unix(ts/1000, (ts%1000)*1e6)
		} else {
			timestamp = time.Unix(ts, 0)
		}
	}

	// Skip stale events
	if time.Since(timestamp) > callEventMaxAge {
		da.log.Debug().Time("timestamp", timestamp).Msg("Skipping stale call event")
		return
	}

	callerNormalized := formatPhoneNumber(callerNumber)
	portalKey := da.portalKeyForCall(evt, callerNormalized)

	sender := callEventSender(evt, callerNormalized)

	// Fetch the contact's real display name from the Dialpad API.
	// The Ably event's contact_name may just be a formatted phone number,
	// but the API returns the actual saved name (e.g., after a rename).
	if evt.Call != nil && evt.Call.ContactKey != "" {
		go da.syncContactName(callerNormalized, evt.Call.ContactKey)
	} else if contactName := evt.ContactName; contactName != "" {
		// Fallback: use the Ably event's contact_name if no contact_key
		cleaned := cleanPhoneNumber(contactName)
		isJustPhone := cleaned == callerNormalized || cleaned == callerNumber
		if !isJustPhone && contactName != callerNumber {
			go da.updateGhostName(callerNormalized, contactName)
		}
	}

	// Build a unique message ID for the call event
	messageID := networkid.MessageID(fmt.Sprintf("call-%d-%s", evt.CallUUID, evt.State))

	remoteEvt := &simplevent.Message[*dialgo.CallEvent]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    portalKey,
			Sender:       sender,
			CreatePortal: true,
			Timestamp:    timestamp,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.
					Int64("call_uuid", evt.CallUUID).
					Str("call_state", evt.State).
					Str("caller", callerNumber)
			},
		},
		Data:               evt,
		ID:                 messageID,
		ConvertMessageFunc: convertCallStart,
	}

	da.login.QueueRemoteEvent(remoteEvt)
}

func callEventSender(evt *dialgo.CallEvent, externalNumber string) bridgev2.EventSender {
	if evt.GetDirection() == "outbound" {
		return bridgev2.EventSender{IsFromMe: true}
	}
	return bridgev2.EventSender{Sender: networkid.UserID(externalNumber)}
}

// handleActiveCallCheck polls /api/activecalls/me and dispatches a CallEvent
// for each ringing inbound call. This is triggered when a delta event signals
// that the user's presence changed to "on_call".
//
// Flow (reverse-engineered from HAR):
//
//	Delta: presence → "on_call"  →  GET /api/activecalls/me  →  CallEvent
//
// The active calls API returns the full call data including the caller's
// phone number, contact display name, and contact key — data that is NOT
// available in the delta event itself.
func (da *DialpadAPI) handleActiveCallCheck() {
	ctx := da.bgCtx

	calls, err := da.client.GetActiveCalls(ctx)
	if err != nil {
		da.log.Warn().Err(err).Msg("Failed to poll active calls")
		return
	}

	da.log.Debug().Int("count", len(calls)).Msg("Polled active calls")

	for _, call := range calls {
		// Only bridge inbound ringing calls
		if call.Direction != "inbound" || call.State != "ringing" {
			continue
		}

		// Dedup: skip if we've already dispatched ringing for this call ID
		callKey := fmt.Sprintf("%d", call.ID)
		if _, loaded := da.activeCallIDs.LoadOrStore(callKey, struct{}{}); loaded {
			da.log.Debug().Int64("call_id", call.ID).Msg("Already dispatched ringing for this call, skipping")
			continue
		}

		// Extract caller info from the API response
		callerNumber := call.ExternalEndpoint
		contactName := ""
		contactKey := call.ContactKey

		if call.Contact != nil {
			if call.Contact.DisplayName != "" {
				contactName = call.Contact.DisplayName
			}
			if call.Contact.ContactKey != "" {
				contactKey = call.Contact.ContactKey
			}
		}

		da.log.Info().
			Int64("call_id", call.ID).
			Str("caller", callerNumber).
			Str("contact_name", contactName).
			Str("direction", call.Direction).
			Str("state", call.State).
			Msg("Dispatching ringing call event from active calls API")

		// Build a real CallEvent with the data from the REST API
		evt := &dialgo.CallEvent{
			Type:        "call",
			CallUUID:    call.ID,
			State:       dialgo.CallStateRinging,
			ContactName: contactName,
			Call: &dialgo.CallDetail{
				Direction:      call.Direction,
				CallerID:       callerNumber,
				TargetCallerID: call.EntryPointDID, // Which of the user's lines was called
				ContactKey:     contactKey,
				DateStarted:    call.DateStarted,
			},
		}

		da.handleCallEvent(evt)
	}
}

// handleVoicemailCheck is a fallback for the legacy state="voicemail" event
// path. Modern Dialpad delivers the recording inline on the hangup event with
// VM=1 — handleCallEvent calls emitVoicemailRecording directly in that case.
// This path remains in case some configurations still send a mid-recording
// voicemail state event; it polls GET /api/call/{call_id} for the URL.
func (da *DialpadAPI) handleVoicemailCheck(evt *dialgo.CallEvent) {
	ctx := da.bgCtx

	if evt.GetExternalNumber() == "" {
		da.log.Debug().
			Int64("call_uuid", evt.CallUUID).
			Msg("Voicemail event has no phone number, skipping")
		return
	}

	// Wait for the caller to finish recording and Dialpad to process the audio.
	delays := []time.Duration{10 * time.Second, 15 * time.Second, 20 * time.Second}

	var recording *dialgo.CallRecording
	for i, delay := range delays {
		time.Sleep(delay)

		callDetail, err := da.client.GetCallByID(ctx, evt.CallUUID)
		if err != nil {
			da.log.Warn().
				Err(err).
				Int64("call_uuid", evt.CallUUID).
				Int("attempt", i+1).
				Msg("Failed to fetch call detail for voicemail")
			continue
		}

		if callDetail.Recording != nil && callDetail.Recording.RecordingURL != "" {
			recording = callDetail.Recording
			break
		}
	}

	if recording == nil {
		da.log.Info().
			Int64("call_uuid", evt.CallUUID).
			Msg("No voicemail recording found after retries")
		return
	}

	da.emitVoicemailRecording(evt, recording)
}

// emitVoicemailRecording downloads the voicemail MP3 from Dialpad and emits
// a Matrix m.audio voice note to the call's portal. Called from two paths:
//   - handleCallEvent (modern): inline recording on the hangup event (VM=1)
//   - handleVoicemailCheck (legacy fallback): recording polled from /api/call/{id}
func (da *DialpadAPI) emitVoicemailRecording(evt *dialgo.CallEvent, recording *dialgo.CallRecording) {
	ctx := da.bgCtx

	callerNumber := cleanPhoneNumber(evt.GetExternalNumber())
	if callerNumber == "" {
		da.log.Debug().
			Int64("call_uuid", evt.CallUUID).
			Msg("Cannot emit voicemail: no caller number")
		return
	}

	da.log.Info().
		Int64("call_uuid", evt.CallUUID).
		Str("recording_url", recording.RecordingURL).
		Int("duration", recording.Duration).
		Str("transcription", recording.TranscriptionText).
		Msg("Downloading voicemail recording")

	audioData, err := da.client.DownloadMedia(ctx, recording.RecordingURL)
	if err != nil {
		da.log.Err(err).
			Int64("call_uuid", evt.CallUUID).
			Str("url", recording.RecordingURL).
			Msg("Failed to download voicemail recording")
		return
	}

	da.log.Info().
		Int64("call_uuid", evt.CallUUID).
		Int("size_bytes", len(audioData)).
		Msg("Downloaded voicemail recording")

	callerNormalized := formatPhoneNumber(callerNumber)
	portalKey := da.portalKeyForCall(evt, callerNormalized)

	sender := bridgev2.EventSender{
		Sender: networkid.UserID(callerNormalized),
	}

	messageID := networkid.MessageID(fmt.Sprintf("vm-%d", evt.CallUUID))

	capturedAudio := audioData
	capturedDuration := recording.Duration

	remoteEvt := &simplevent.Message[*dialgo.CallEvent]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    portalKey,
			Sender:       sender,
			CreatePortal: true,
			Timestamp:    time.Now(),
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.
					Int64("call_uuid", evt.CallUUID).
					Str("call_state", "voicemail").
					Str("caller", callerNumber)
			},
		},
		Data: evt,
		ID:   messageID,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *dialgo.CallEvent) (*bridgev2.ConvertedMessage, error) {
			mimeType := http.DetectContentType(capturedAudio)
			if mimeType == "application/octet-stream" {
				mimeType = "audio/mpeg"
			}
			uri, encFile, err := intent.UploadMedia(ctx, "", capturedAudio, "", mimeType)
			if err != nil {
				return nil, fmt.Errorf("upload voicemail to Matrix: %w", err)
			}
			durationMs := capturedDuration * 1000
			return &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType:  event.MsgAudio,
						Body:     "📞 Voicemail",
						FileName: "voicemail.mp3",
						URL:      uri,
						File:     encFile,
						Info: &event.FileInfo{
							MimeType: mimeType,
							Size:     len(capturedAudio),
							Duration: durationMs,
						},
						MSC3245Voice: &event.MSC3245Voice{},
						MSC1767Audio: &event.MSC1767Audio{
							Duration: durationMs,
						},
					},
				}},
			}, nil
		},
	}

	da.login.QueueRemoteEvent(remoteEvt)
}

// convertCallStart creates the Matrix message content for a call notice.
// Only inbound ringing includes BeeperActionMessage (triggers the
// "Open Dialpad to answer" banner in Beeper). All other states
// (hangup, voicemail, outbound) send plain notices — no banner.
func convertCallStart(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, evt *dialgo.CallEvent) (*bridgev2.ConvertedMessage, error) {
	var text string
	isOutbound := evt.GetDirection() == "outbound"
	isInboundRinging := evt.State == dialgo.CallStateRinging && !isOutbound

	// connectedSeconds returns how long the parties were actually connected
	// (DateEnded - DateConnected, in seconds). Returns 0 if the call never
	// connected. Dialpad's `Duration` field includes ring time and is NOT a
	// reliable "was answered?" signal.
	connectedSeconds := func() int {
		if evt.Call == nil || evt.Call.DateConnected == 0 {
			return 0
		}
		end := evt.Call.DateEnded
		if end == 0 {
			end = time.Now().UnixMilli()
		}
		secs := (end - evt.Call.DateConnected) / 1000
		if secs < 0 {
			return 0
		}
		return int(secs)
	}
	wasAnswered := evt.Call != nil && evt.Call.DateConnected > 0

	switch {
	case evt.State == dialgo.CallStateHangup && isOutbound:
		if d := connectedSeconds(); d > 0 {
			text = fmt.Sprintf("📞 Outgoing call (%d seconds)", d)
		} else {
			text = "📞 Outgoing call"
		}
	case evt.State == dialgo.CallStateHangup:
		// Inbound hangup. "Missed" if Dialpad tagged it OR the parties never
		// connected. Duration > 0 just means it rang — not that anyone picked up.
		if evt.Missed == 1 || !wasAnswered {
			text = "📞 Missed call"
		} else if d := connectedSeconds(); d > 0 {
			text = fmt.Sprintf("📞 Call ended (%d seconds)", d)
		} else {
			text = "📞 Call ended"
		}
	case isInboundRinging:
		text = "Incoming call. Open Dialpad to answer."
	default:
		text = "📞 Call"
	}

	content := &event.MessageEventContent{
		Body: text,
	}

	if isInboundRinging {
		// Only ringing gets the action message → triggers the banner
		content.MsgType = event.MsgText
		content.BeeperActionMessage = &event.BeeperActionMessage{
			Type:     event.BeeperActionMessageCall,
			CallType: event.BeeperActionMessageCallTypeVoice,
		}
	} else {
		// Everything else is a plain notice — no banner
		content.MsgType = event.MsgNotice
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

// bridgeDeliveryFailure sends a notice to the portal when an SMS/MMS delivery fails.
// The user sees "⚠️ Message delivery failed: <reason>" in the chat.
func (da *DialpadAPI) bridgeDeliveryFailure(evt *dialgo.SMSEvent, reason string) {
	da.log.Warn().
		Int64("id", evt.ID).
		Str("status", evt.MessageStatus).
		Str("reason", reason).
		Msg("SMS delivery failed")

	// Determine the target portal
	var externalNumber string
	if len(evt.ToNumber) > 0 {
		externalNumber = evt.ToNumber[0]
	} else if evt.Contact.PhoneNumber != "" {
		externalNumber = evt.Contact.PhoneNumber
	} else {
		externalNumber = evt.FromNumber
	}
	if externalNumber == "" {
		return
	}

	// For outbound messages, FromNumber is our line
	myNumber := da.getMyNumber(formatPhoneNumber(evt.FromNumber))
	portalKey := networkid.PortalKey{
		ID:       MakeDMPortalID(myNumber, formatPhoneNumber(externalNumber)),
		Receiver: da.login.ID,
	}

	messageID := networkid.MessageID(fmt.Sprintf("fail-%d", evt.ID))

	remoteEvt := &simplevent.Message[*dialgo.SMSEvent]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    portalKey,
			Sender:       bridgev2.EventSender{IsFromMe: true},
			CreatePortal: false, // Don't create a portal just for a failure notice
			Timestamp:    time.Now(),
		},
		Data: evt,
		ID:   messageID,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *dialgo.SMSEvent) (*bridgev2.ConvertedMessage, error) {
			return &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgNotice,
						Body:    fmt.Sprintf("⚠️ Message delivery failed: %s", reason),
					},
				}},
			}, nil
		},
	}

	da.login.QueueRemoteEvent(remoteEvt)
}
