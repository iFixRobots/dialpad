package connector

import (
	"reflect"
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// mergeConversationsByContactKey: the two scope responses get merged in
// UserProfile-then-Office order, deduped by contact_key, with empty
// target_keys on office entries stamped from officeKey.

func TestMergeConversationsByContactKey_DedupesAcrossScopes(t *testing.T) {
	user := []dialgo.FeedContact{
		{ContactKey: "ck-1", DisplayName: "Alice", DialString: "+14155550001"},
		{ContactKey: "ck-2", DisplayName: "Bob", DialString: "+14155550002"},
	}
	office := []dialgo.FeedContact{
		{ContactKey: "ck-2", DisplayName: "Bob (office)", DialString: "+14155550002"}, // dup
		{ContactKey: "ck-3", DisplayName: "Carol", DialString: "+14155550003"},
	}
	got := mergeConversationsByContactKey(user, office, "office-key-1")
	if len(got) != 3 {
		t.Fatalf("merged length = %d; want 3 (ck-1, ck-2 from user, ck-3 from office)", len(got))
	}
	gotKeys := []string{got[0].ContactKey, got[1].ContactKey, got[2].ContactKey}
	wantKeys := []string{"ck-1", "ck-2", "ck-3"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("merged order/keys = %v; want %v", gotKeys, wantKeys)
	}
	// The UserProfile entry for ck-2 should survive, not the office one.
	if got[1].DisplayName != "Bob" {
		t.Errorf("ck-2 display = %q; want %q (UserProfile entry should win on dup)", got[1].DisplayName, "Bob")
	}
}

func TestMergeConversationsByContactKey_StampsOfficeTargetKey(t *testing.T) {
	office := []dialgo.FeedContact{
		{ContactKey: "ck-grp", Type: "contact_group"}, // TargetKey empty
		{ContactKey: "ck-stamped", TargetKey: "already-set"},
	}
	got := mergeConversationsByContactKey(nil, office, "office-key-1")
	if got[0].TargetKey != "office-key-1" {
		t.Errorf("empty TargetKey not stamped: got %q; want %q", got[0].TargetKey, "office-key-1")
	}
	if got[1].TargetKey != "already-set" {
		t.Errorf("populated TargetKey overwritten: got %q; want %q", got[1].TargetKey, "already-set")
	}
}

func TestMergeConversationsByContactKey_KeepsEmptyContactKey(t *testing.T) {
	// Self-reference entries (empty contact_key) are kept by merge — they get
	// filtered later by classifyDMConv at sync time. Without that, two
	// distinct self-ref rows in the same scope would clobber each other.
	user := []dialgo.FeedContact{
		{ContactKey: "", DisplayName: "Self 1"},
		{ContactKey: "", DisplayName: "Self 2"},
	}
	got := mergeConversationsByContactKey(user, nil, "")
	if len(got) != 2 {
		t.Errorf("empty-contact-key entries got deduped; len=%d want 2", len(got))
	}
}

// classifyDMConv: the self-reference filter, exercised over its three exit
// conditions plus the happy path.

func TestClassifyDMConv(t *testing.T) {
	ownLines := map[string]bool{
		"+14155550100": true, // user's primary
		"+14155550101": true, // user's office DID
	}
	cases := []struct {
		name           string
		conv           dialgo.FeedContact
		wantPhone      string
		wantSkipReason string
	}{
		{
			name:           "happy path — external contact with E.164 dial string",
			conv:           dialgo.FeedContact{ContactKey: "ck-1", DialString: "+14155550001"},
			wantPhone:      "+14155550001",
			wantSkipReason: "",
		},
		{
			name:           "happy path — falls back to PrimaryPhone when DialString empty",
			conv:           dialgo.FeedContact{ContactKey: "ck-1", PrimaryPhone: "+14155550001"},
			wantPhone:      "+14155550001",
			wantSkipReason: "",
		},
		{
			name:           "skip — empty contact_key (self-reference row from /api/contact/)",
			conv:           dialgo.FeedContact{ContactKey: "", DialString: "+14155550001"},
			wantPhone:      "",
			wantSkipReason: "empty contact_key",
		},
		{
			name:           "skip — no phone at all",
			conv:           dialgo.FeedContact{ContactKey: "ck-1"},
			wantPhone:      "",
			wantSkipReason: "no phone",
		},
		{
			name:           "skip — phone matches the user's primary line",
			conv:           dialgo.FeedContact{ContactKey: "ck-1", DialString: "+14155550100"},
			wantPhone:      "+14155550100",
			wantSkipReason: "phone matches own line",
		},
		{
			name:           "skip — phone matches an office DID",
			conv:           dialgo.FeedContact{ContactKey: "ck-1", PrimaryPhone: "+14155550101"},
			wantPhone:      "+14155550101",
			wantSkipReason: "phone matches own line",
		},
		{
			name:           "happy path — non-E.164 input gets normalized through formatPhoneNumber",
			conv:           dialgo.FeedContact{ContactKey: "ck-1", DialString: "(415) 555-0001"},
			wantPhone:      "+14155550001",
			wantSkipReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			phone, skip := classifyDMConv(tc.conv, ownLines)
			if phone != tc.wantPhone {
				t.Errorf("phone = %q; want %q", phone, tc.wantPhone)
			}
			if skip != tc.wantSkipReason {
				t.Errorf("skipReason = %q; want %q", skip, tc.wantSkipReason)
			}
		})
	}
}

// resolveGroupParticipants: drops own lines, drops unparseable entries,
// sorts the result, and returns nil if fewer than 2 externals remain.

func TestResolveGroupParticipants(t *testing.T) {
	ownLines := map[string]bool{
		"+14155550100": true,
		"+14155550101": true,
	}
	cases := []struct {
		name   string
		phones []string
		want   []string // nil = should be skipped
	}{
		{
			name:   "two externals, already sorted",
			phones: []string{"+14155550001", "+14155550002"},
			want:   []string{"+14155550001", "+14155550002"},
		},
		{
			name:   "two externals, unsorted input — result is sorted",
			phones: []string{"+14155550002", "+14155550001"},
			want:   []string{"+14155550001", "+14155550002"},
		},
		{
			name:   "own line mixed in is dropped",
			phones: []string{"+14155550001", "+14155550100", "+14155550002"},
			want:   []string{"+14155550001", "+14155550002"},
		},
		{
			name:   "only one external after filtering own lines — skipped",
			phones: []string{"+14155550001", "+14155550100", "+14155550101"},
			want:   nil,
		},
		{
			name:   "all phones are own lines — skipped",
			phones: []string{"+14155550100", "+14155550101"},
			want:   nil,
		},
		{
			name: "unparseable entries are passed through — formatPhoneNumber returns the " +
				"original string when parsing fails, so we trust the upstream list",
			phones: []string{"+14155550001", "not-a-number", "+14155550002"},
			// Sort order: "+14155550001" < "+14155550002" < "not-a-number"
			want: []string{"+14155550001", "+14155550002", "not-a-number"},
		},
		{
			name:   "empty phones — skipped",
			phones: nil,
			want:   nil,
		},
		{
			name:   "empty-string entries are dropped",
			phones: []string{"+14155550001", "", "+14155550002"},
			want:   []string{"+14155550001", "+14155550002"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGroupParticipants(tc.phones, ownLines)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("participants = %v; want %v", got, tc.want)
			}
		})
	}
}

// Integration-ish: the merged + filtered pipeline a sync run takes — fed a
// realistic mix of UserProfile + Office responses, what does the bridge
// end up emitting? This locks in the behaviour the user observed at
// recycle-time (18 conversations, no phantom "Rodrz" rooms).
func TestSyncPipeline_FiltersAndMerges(t *testing.T) {
	ownLines := map[string]bool{
		"+14155550100": true,
		"+14155550101": true,
	}
	user := []dialgo.FeedContact{
		{ContactKey: "ck-1", DialString: "+14155550001", DisplayName: "Alice"},
		{ContactKey: "", DialString: "+14155550100", DisplayName: "Rodrz (self-ref empty key)"},
		{ContactKey: "ck-dup", DialString: "+14155550002", DisplayName: "Bob (user)"},
	}
	office := []dialgo.FeedContact{
		{ContactKey: "ck-dup", DialString: "+14155550002", DisplayName: "Bob (office) — should be dropped"},
		{ContactKey: "ck-self", DialString: "+14155550101", DisplayName: "Phantom self-ref by phone match"},
		{ContactKey: "ck-grp", Type: "contact_group", Phones: []string{"+14155550003", "+14155550004"}, DisplayName: "Group"},
	}

	merged := mergeConversationsByContactKey(user, office, "office-key-1")

	var emittedDMs []string
	var emittedGroups []networkid.PortalID
	for _, conv := range merged {
		if conv.IsGroup() {
			parts := resolveGroupParticipants(conv.Phones, ownLines)
			if parts == nil {
				continue
			}
			emittedGroups = append(emittedGroups, MakeGroupPortalID(parts))
			continue
		}
		phone, skip := classifyDMConv(conv, ownLines)
		if skip != "" {
			continue
		}
		// In real code this becomes MakeDMPortalID(myNumber, phone); for the
		// pipeline test the phone identifies which contacts survived.
		emittedDMs = append(emittedDMs, phone)
	}

	wantDMs := []string{"+14155550001", "+14155550002"}
	if !reflect.DeepEqual(emittedDMs, wantDMs) {
		t.Errorf("emitted DMs = %v; want %v (self-ref by empty key + self-ref by phone should be dropped)",
			emittedDMs, wantDMs)
	}
	wantGroups := []networkid.PortalID{"group:+14155550003,+14155550004"}
	if !reflect.DeepEqual(emittedGroups, wantGroups) {
		t.Errorf("emitted groups = %v; want %v", emittedGroups, wantGroups)
	}
}
