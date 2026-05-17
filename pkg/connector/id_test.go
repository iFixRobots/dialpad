package connector

import (
	"reflect"
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestMakeDMPortalID(t *testing.T) {
	got := MakeDMPortalID("+14155550100", "+14155550104")
	want := networkid.PortalID("sms:+14155550100:+14155550104")
	if got != want {
		t.Errorf("MakeDMPortalID = %q; want %q", got, want)
	}
}

func TestMakeGroupPortalID(t *testing.T) {
	got := MakeGroupPortalID([]string{"+14155550103", "+14155550102"})
	want := networkid.PortalID("group:+14155550103,+14155550102")
	if got != want {
		t.Errorf("MakeGroupPortalID = %q; want %q", got, want)
	}
}

func TestIsGroupPortalID(t *testing.T) {
	cases := []struct {
		in   networkid.PortalID
		want bool
	}{
		{"sms:+1:+2", false},
		{"group:+1,+2", true},
		{"", false},
		{"unknown:foo", false},
	}
	for _, tc := range cases {
		if got := IsGroupPortalID(tc.in); got != tc.want {
			t.Errorf("IsGroupPortalID(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestParsePortalID(t *testing.T) {
	cases := []struct {
		name        string
		in          networkid.PortalID
		wantKind    portalKind
		wantMy      string
		wantOther   string
		wantPhones  []string
		wantOK      bool
	}{
		{
			name:      "DM round-trip",
			in:        MakeDMPortalID("+14155550100", "+14155550104"),
			wantKind:  portalKindDM,
			wantMy:    "+14155550100",
			wantOther: "+14155550104",
			wantOK:    true,
		},
		{
			name:       "Group round-trip",
			in:         MakeGroupPortalID([]string{"+14155550103", "+14155550102"}),
			wantKind:   portalKindGroup,
			wantPhones: []string{"+14155550103", "+14155550102"},
			wantOK:     true,
		},
		{
			name:       "Group with 3 participants",
			in:         "group:+1,+2,+3",
			wantKind:   portalKindGroup,
			wantPhones: []string{"+1", "+2", "+3"},
			wantOK:     true,
		},
		{
			name:     "Empty input",
			in:       "",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "Truncated DM (only one part)",
			in:       "sms:+14155550100",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "DM with empty 'my' side",
			in:       "sms::+14155550104",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "DM with empty 'other' side",
			in:       "sms:+14155550100:",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "Group with no phones",
			in:       "group:",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "Group with trailing comma (empty phone)",
			in:       "group:+1,",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "Unknown prefix",
			in:       "unknown:foo:bar",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
		{
			name:     "Bare colon",
			in:       ":",
			wantKind: portalKindUnknown,
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, my, other, phones, ok := ParsePortalID(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tc.wantOK)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %v; want %v", kind, tc.wantKind)
			}
			if my != tc.wantMy {
				t.Errorf("my = %q; want %q", my, tc.wantMy)
			}
			if other != tc.wantOther {
				t.Errorf("other = %q; want %q", other, tc.wantOther)
			}
			if !reflect.DeepEqual(phones, tc.wantPhones) {
				t.Errorf("phones = %#v; want %#v", phones, tc.wantPhones)
			}
		})
	}
}

// Confirms the byte-identical output guarantee — the new helpers produce the
// same strings as the legacy fmt.Sprintf/Join idioms scattered through the
// codebase before this refactor. A regression here means existing portal
// rows in the DB would be orphaned.
func TestPortalIDByteIdenticalToLegacyFormat(t *testing.T) {
	// DM: "sms:%s:%s"
	if got := string(MakeDMPortalID("+14155550100", "+14155550104")); got != "sms:+14155550100:+14155550104" {
		t.Errorf("DM format drifted: %q", got)
	}
	// Group: "group:" + strings.Join(phones, ",")
	if got := string(MakeGroupPortalID([]string{"+14155550103", "+14155550102"})); got != "group:+14155550103,+14155550102" {
		t.Errorf("Group format drifted: %q", got)
	}
}
