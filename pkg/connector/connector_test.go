package connector

import (
	"testing"
)

func TestFormatPhoneNumber(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"+14155550102", "+14155550102"},
		{"(415) 555-0102", "+14155550102"},
		{"4155550102", "+14155550102"},
		{"415.555.0102", "+14155550102"},
		{"+44 20 7946 0958", "+442079460958"},
		{"not a phone", "not a phone"},
	}
	for _, tc := range cases {
		got := formatPhoneNumber(tc.in)
		if got != tc.want {
			t.Errorf("formatPhoneNumber(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanPhoneNumber(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"+14155550102", "+14155550102"},
		{"(415) 555-0102", "4155550102"},
		{"+1 (415) 555-0102", "+14155550102"},
		{"foo bar", ""},
		{"123", ""},
	}
	for _, tc := range cases {
		got := cleanPhoneNumber(tc.in)
		if got != tc.want {
			t.Errorf("cleanPhoneNumber(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// Portal-ID make/parse coverage moved to id_test.go alongside the helpers
// themselves. See TestMakeDMPortalID, TestMakeGroupPortalID, TestParsePortalID
// for round-trip + malformed-input cases.

func TestMakeRoomTopic(t *testing.T) {
	if topic := makeRoomTopic(""); topic != "Dialpad" {
		t.Errorf("makeRoomTopic(\"\") = %q; want %q", topic, "Dialpad")
	}
	topic := makeRoomTopic("+14155550100")
	if topic == "" || topic == "Dialpad" {
		t.Errorf("makeRoomTopic with phone returned bare default: %q", topic)
	}
}

func TestGetMyNumberPriority(t *testing.T) {
	cases := []struct {
		name        string
		meta        UserLoginMetadata
		hint        string
		want        string
	}{
		{
			name: "hint wins over everything",
			meta: UserLoginMetadata{PreferredLine: "+14155550100", PrimaryPhone: "+14155550101"},
			hint: "+1555",
			want: "+1555",
		},
		{
			name: "preferred line beats primary",
			meta: UserLoginMetadata{PreferredLine: "+14155550100", PrimaryPhone: "+14155550101"},
			want: "+14155550100",
		},
		{
			name: "primary used when no preference",
			meta: UserLoginMetadata{PrimaryPhone: "+14155550101", Phones: []string{"+14155550101", "+14155550100"}},
			want: "+14155550101",
		},
		{
			name: "first phone if neither set",
			meta: UserLoginMetadata{Phones: []string{"+14155550101", "+14155550100"}},
			want: "+14155550101",
		},
		{
			name: "empty when nothing configured",
			meta: UserLoginMetadata{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			da := &DialpadAPI{meta: &tc.meta}
			got := da.getMyNumber(tc.hint)
			if got != tc.want {
				t.Errorf("getMyNumber(%q) = %q; want %q", tc.hint, got, tc.want)
			}
		})
	}
}
