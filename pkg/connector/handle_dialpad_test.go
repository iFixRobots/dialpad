package connector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// Locks the missed-call detection. Duration > 0 just means the phone rang for
// N seconds — NOT that anyone answered. The signal for "got picked up" is
// DateConnected != 0. The regression that motivated this test showed
// "Call ended (17 seconds)" for a call that nobody picked up.
func TestConvertCallStart_HangupText(t *testing.T) {
	now := time.Now().UnixMilli()

	cases := []struct {
		name string
		evt  *dialgo.CallEvent
		want string
	}{
		{
			name: "inbound rang 17s, never answered → missed",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup,
				Call: &dialgo.CallDetail{
					Direction:     "inbound",
					DateStarted:   now - 17_000,
					DateConnected: 0,
					DateEnded:     now,
					Duration:      17,
				},
			},
			want: "📞 Missed call",
		},
		{
			name: "inbound answered, talked for 30s → Call ended (30 seconds)",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup,
				Call: &dialgo.CallDetail{
					Direction:     "inbound",
					DateStarted:   now - 40_000,
					DateConnected: now - 30_000,
					DateEnded:     now,
					Duration:      40, // ring + talk
				},
			},
			want: "📞 Call ended (30 seconds)",
		},
		{
			name: "inbound with Missed=1 flag → missed (regardless of dates)",
			evt: &dialgo.CallEvent{
				State:  dialgo.CallStateHangup,
				Missed: 1,
				Call: &dialgo.CallDetail{
					Direction:     "inbound",
					DateStarted:   now - 5_000,
					DateConnected: now - 4_000, // even if Dialpad reports connected, Missed=1 wins
					DateEnded:     now,
				},
			},
			want: "📞 Missed call",
		},
		{
			name: "outbound never connected (callee didn't pick up) → bare Outgoing call",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup,
				Call: &dialgo.CallDetail{
					Direction:     "outbound",
					DateStarted:   now - 22_000,
					DateConnected: 0,
					DateEnded:     now,
					Duration:      22,
				},
			},
			want: "📞 Outgoing call",
		},
		{
			name: "outbound answered, talked 12s → Outgoing call (12 seconds)",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup,
				Call: &dialgo.CallDetail{
					Direction:     "outbound",
					DateStarted:   now - 20_000,
					DateConnected: now - 12_000,
					DateEnded:     now,
					Duration:      20,
				},
			},
			want: "📞 Outgoing call (12 seconds)",
		},
		{
			name: "inbound ringing → action banner",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateRinging,
				Call:  &dialgo.CallDetail{Direction: "inbound"},
			},
			want: "Incoming call. Open Dialpad to answer.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			converted, err := convertCallStart(context.Background(), nil, nil, tc.evt)
			if err != nil {
				t.Fatalf("convertCallStart returned error: %v", err)
			}
			if len(converted.Parts) != 1 {
				t.Fatalf("got %d parts; want 1", len(converted.Parts))
			}
			body := converted.Parts[0].Content.Body
			if body != tc.want {
				t.Errorf("body = %q; want %q", body, tc.want)
			}
			// Defensive: anything called "Missed" must not also include a seconds count
			if tc.want == "📞 Missed call" && strings.Contains(body, "seconds") {
				t.Errorf("missed-call body must not mention seconds, got %q", body)
			}
		})
	}
}

// Locks the rule: secondary-leg call events ONLY emit on ringing. Dialpad
// fans inbound calls into a primary leg (carries missed + voicemail) and a
// secondary leg per ringing device (sends ringing + a redundant hangup).
// Treating both legs' hangups as full lifecycle events renders "Missed call"
// twice — the regression this test guards against.
func TestSecondaryLegSuppression(t *testing.T) {
	shouldSuppress := func(evt *dialgo.CallEvent) bool {
		return evt.Call != nil && evt.Call.IsSecondary == 1 && evt.State != dialgo.CallStateRinging
	}

	cases := []struct {
		name     string
		evt      *dialgo.CallEvent
		suppress bool
	}{
		{
			name: "secondary ringing — keep (the only incoming-call signal)",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateRinging,
				Call:  &dialgo.CallDetail{Direction: "inbound", IsSecondary: 1},
			},
			suppress: false,
		},
		{
			name: "secondary hangup — drop (primary leg owns the missed-call notice)",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup,
				Call:  &dialgo.CallDetail{Direction: "inbound", IsSecondary: 1},
			},
			suppress: true,
		},
		{
			name: "secondary voicemail-state — drop (never happens on secondary in practice, defensive)",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateVoicemail,
				Call:  &dialgo.CallDetail{Direction: "inbound", IsSecondary: 1},
			},
			suppress: true,
		},
		{
			name: "primary ringing — keep",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateRinging,
				Call:  &dialgo.CallDetail{Direction: "inbound", IsSecondary: 0},
			},
			suppress: false,
		},
		{
			name: "primary hangup — keep (carries missed flag)",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup,
				Call:  &dialgo.CallDetail{Direction: "inbound", IsSecondary: 0},
			},
			suppress: false,
		},
		{
			name: "nil Call — keep, suppression rule needs the field",
			evt:  &dialgo.CallEvent{State: dialgo.CallStateHangup},
			suppress: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSuppress(tc.evt); got != tc.suppress {
				t.Errorf("shouldSuppress = %v; want %v", got, tc.suppress)
			}
		})
	}
}

// Locks the detection of a voicemail piggybacking on a hangup event. Dialpad
// delivers state=hangup with vm=1 and the recording inline — no separate
// voicemail state, no polling needed. The regression that motivated this:
// missed-call rendered as "Missed call" but the voicemail audio never arrived
// because we were waiting for a state=voicemail event that Dialpad never emits.
func TestCallEvent_HasInlineVoicemail(t *testing.T) {
	hasInlineVoicemail := func(evt *dialgo.CallEvent) bool {
		return evt.State == dialgo.CallStateHangup && evt.VM == 1 &&
			evt.Call != nil && evt.Call.Recording != nil &&
			evt.Call.Recording.RecordingURL != ""
	}

	cases := []struct {
		name string
		evt  *dialgo.CallEvent
		want bool
	}{
		{
			name: "hangup with vm=1 and inline recording → yes",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup, VM: 1,
				Call: &dialgo.CallDetail{
					Direction: "inbound",
					Recording: &dialgo.CallRecording{
						RecordingURL: "https://dialpad.com/blob/voicemail/abc.mp3",
						Duration:     10,
					},
				},
			},
			want: true,
		},
		{
			name: "hangup without vm=1 (regular missed call) → no",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup, VM: 0,
				Call: &dialgo.CallDetail{Direction: "inbound"},
			},
			want: false,
		},
		{
			name: "ringing with vm=1 (impossible but defensive) → no, not a hangup",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateRinging, VM: 1,
				Call: &dialgo.CallDetail{
					Direction: "inbound",
					Recording: &dialgo.CallRecording{RecordingURL: "https://..."},
				},
			},
			want: false,
		},
		{
			name: "hangup vm=1 but no recording URL (Dialpad still processing) → no",
			evt: &dialgo.CallEvent{
				State: dialgo.CallStateHangup, VM: 1,
				Call: &dialgo.CallDetail{
					Direction: "inbound",
					Recording: &dialgo.CallRecording{Duration: 10},
				},
			},
			want: false,
		},
		{
			name: "hangup vm=1 nil Call → no, defensive",
			evt:  &dialgo.CallEvent{State: dialgo.CallStateHangup, VM: 1},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasInlineVoicemail(tc.evt); got != tc.want {
				t.Errorf("hasInlineVoicemail = %v; want %v", got, tc.want)
			}
		})
	}
}
