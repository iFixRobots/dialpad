package dialgo

// CallEvent represents a call event received via Ably push.
// The payload is structured differently from the v2 API subscription format.
//
// Example Ably push payload:
//
//	{
//	  "type": "call",
//	  "call_uuid": 6195841232412672,
//	  "state": "connected",
//	  "contact_name": "(415) 555-0102",
//	  "feed_key": "FeedItem:Call:6195841232412672_UserProfile:6682319528009728",
//	  "call": {
//	    "contact_key": "agxzfnViZXItdm9pY2Vy...",
//	    "direction": "inbound",
//	    "duration": 5,
//	    "caller_id": "+14155550102",
//	    "target_caller_id": "+14155550100"
//	  }
//	}
type CallEvent struct {
	Type        string `json:"type"`         // "call"
	CallUUID    int64  `json:"call_uuid"`    // Numeric call identifier
	State       string `json:"state"`        // "ringing", "connected", "hangup", "voicemail"
	Context     string `json:"context"`      // Additional context (often null)
	ContactName string `json:"contact_name"` // Display name or phone of external party
	FeedKey     string `json:"feed_key"`     // Feed item key
	Missed      int    `json:"missed"`       // 1 = missed call (set on hangup events)
	Title       string `json:"title"`        // e.g. "Missed call" (set by Dialpad on hangup)

	// VM=1 marks a hangup event that has an associated voicemail recording.
	// Dialpad does NOT emit a separate state="voicemail" event; the full
	// recording (URL + transcription) is delivered inline as part of the
	// single hangup event when VM==1. Verified against both bridge logs
	// and a HAR capture of the official web client.
	VM int `json:"vm"`

	// The embedded call detail object
	Call *CallDetail `json:"call,omitempty"`
}

// CallDetail contains the nested call data within an Ably push event.
type CallDetail struct {
	ContactKey        string         `json:"contact_key"`        // Datastore key for the contact
	Direction         string         `json:"direction"`          // "inbound" or "outbound"
	Duration          int            `json:"duration"`           // Total wall-time including ring (NOT a proxy for "was answered")
	DurationConnected int            `json:"duration_connected"` // Connected (talk) duration; 0 if never answered
	Category          string         `json:"category"`           // "voicemail", "missed", "incoming", "outgoing", "cancelled"
	CallerID          string         `json:"caller_id"`          // E.164 phone of caller
	TargetCallerID    string         `json:"target_caller_id"`   // E.164 phone of callee (Dialpad user)
	FeedTarget        string         `json:"feed_target"`        // Datastore key of the target user
	ExternalEndpoint  string         `json:"external_endpoint"`  // E.164 of the external party
	DateStarted       int64          `json:"date_started"`       // Epoch millis
	DateConnected     int64          `json:"date_connected"`     // Epoch millis; 0 if never answered
	DateEnded         int64          `json:"date_ended"`         // Epoch millis
	Recording         *CallRecording `json:"recording,omitempty"` // Populated on hangup events with VM=1
	// IsSecondary=1 marks a device-ring leg: Dialpad fans one inbound call
	// into a primary leg (the call from the entry-point perspective; sends
	// hangup/voicemail events) and one secondary leg per ringing device
	// (sends ringing/hangup events). Treating both as full call lifecycles
	// renders the missed-call notice twice. Use IsSecondary to suppress the
	// redundant hangup from the secondary leg.
	IsSecondary int `json:"is_secondary"`
}

// Call states as observed in Ably push events.
const (
	CallStateRinging   = "ringing"
	CallStateConnected = "connected"
	CallStateHangup    = "hangup"
	CallStateVoicemail = "voicemail"
)

// GetExternalNumber returns the phone number of the external party.
// Field-priority order reflects what current Dialpad push events actually
// send: external_endpoint is the canonical field on hangup/voicemail events;
// caller_id / target_caller_id are only present on some legacy shapes.
// ContactName is a last-ditch fallback and may be a display name (not a
// phone), so callers must still treat the result as a candidate string.
func (e *CallEvent) GetExternalNumber() string {
	if e.Call != nil {
		if e.Call.ExternalEndpoint != "" {
			return e.Call.ExternalEndpoint
		}
		if e.Call.Direction == "inbound" && e.Call.CallerID != "" {
			return e.Call.CallerID
		}
		if e.Call.Direction == "outbound" && e.Call.TargetCallerID != "" {
			return e.Call.TargetCallerID
		}
		if e.Call.CallerID != "" {
			return e.Call.CallerID
		}
	}
	return e.ContactName
}

// GetContactKey returns the Dialpad contact_key associated with this call,
// when present. Used to route call events to an existing portal that was
// created for the same contact (e.g. an SMS conversation) so calls don't
// spawn duplicate rooms.
func (e *CallEvent) GetContactKey() string {
	if e.Call != nil {
		return e.Call.ContactKey
	}
	return ""
}

// GetDirection returns inbound/outbound.
func (e *CallEvent) GetDirection() string {
	if e.Call != nil && e.Call.Direction != "" {
		return e.Call.Direction
	}
	return "inbound" // default assumption
}

// GetMyNumber returns the user's Dialpad phone number involved in this call.
// For inbound: target_caller_id is the user's line that was called.
// For outbound: caller_id is the user's line that placed the call.
func (e *CallEvent) GetMyNumber() string {
	if e.Call != nil {
		if e.Call.Direction == "inbound" && e.Call.TargetCallerID != "" {
			return e.Call.TargetCallerID
		}
		if e.Call.Direction == "outbound" && e.Call.CallerID != "" {
			return e.Call.CallerID
		}
	}
	return ""
}
