package connector

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// newTestAPI returns a DialpadAPI usable in routing tests. Only fields touched
// by newDialpadMessageEvent / portalKeyForContact / getMyNumber are populated.
func newTestAPI(meta UserLoginMetadata, loginID string) *DialpadAPI {
	return &DialpadAPI{
		log:  zerolog.Nop(),
		meta: &meta,
		login: &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: networkid.UserLoginID(loginID)},
		},
	}
}

func TestMakeTextHashDeterministic(t *testing.T) {
	a := makeTextHash("sms:+14155550100:+14159389005", "hello")
	b := makeTextHash("sms:+14155550100:+14159389005", "hello")
	if a != b {
		t.Errorf("hash not deterministic: %q vs %q", a, b)
	}
	c := makeTextHash("sms:+14155550100:+14159389005", "different")
	if a == c {
		t.Error("hash collided for different text")
	}
	if !strings.HasPrefix(string(a), "dp-") {
		t.Errorf("hash missing prefix: %q", a)
	}
}

func TestPortalKeyForContact_EmptyKey(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{PrimaryPhone: "+14155550100"}, "login-1")
	if _, ok := da.portalKeyForContact(""); ok {
		t.Error("expected empty contact_key to miss the cache")
	}
}

func TestPortalKeyForContact_HitAndMiss(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{PrimaryPhone: "+14155550100"}, "login-1")
	want := networkid.PortalKey{ID: "sms:+1:+2", Receiver: "login-1"}
	da.contactKeyToPortal.Store("ck-abc", want)

	got, ok := da.portalKeyForContact("ck-abc")
	if !ok || got != want {
		t.Errorf("hit: got (%v, %v); want (%v, true)", got, ok, want)
	}
	if _, ok := da.portalKeyForContact("ck-missing"); ok {
		t.Error("expected miss for unknown contact_key")
	}
}

// newDialpadMessageEvent: outbound group echo with comma-joined to_phone should
// route via contact_key cache to the GROUP portal, NOT compose a corrupted DM
// portal like sms:my:+ph1,+ph2.
func TestNewDialpadMessageEvent_GroupEchoRoutesViaContactKey(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550100",
		Phones:       []string{"+14155550100", "+14155550101"},
	}, "login-1")
	groupPortal := networkid.PortalKey{
		ID:       "group:+14155550103,+14155550102",
		Receiver: "login-1",
	}
	da.contactKeyToPortal.Store("ck-group", groupPortal)

	evt := &dialgo.SMSEvent{
		ID:         1234,
		Direction:  "outbound",
		FromNumber: "+14155550101",
		ToNumber:   []string{"+14155550103,+14155550102"}, // comma-joined group form
		Text:       "hi group",
		Contact:    dialgo.Contact{ID: "ck-group"},
	}
	msg := da.newDialpadMessageEvent(evt)
	if msg == nil {
		t.Fatal("expected message event, got nil")
	}
	if msg.portalKey != groupPortal {
		t.Errorf("routed to %v; want %v", msg.portalKey, groupPortal)
	}
	if !strings.HasPrefix(string(msg.portalKey.ID), "group:") {
		t.Errorf("expected group: portal, got %q", msg.portalKey.ID)
	}
	if strings.Contains(string(msg.portalKey.ID), ",") &&
		!strings.HasPrefix(string(msg.portalKey.ID), "group:") {
		t.Errorf("portal ID contains comma but is not a group: %q", msg.portalKey.ID)
	}
}

// Without a cached contact_key, an outbound DM event falls back to phone-based
// portal composition (sms:my:their).
func TestNewDialpadMessageEvent_DMFallback(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550100",
		Phones:       []string{"+14155550100"},
	}, "login-1")
	evt := &dialgo.SMSEvent{
		ID:         5678,
		Direction:  "outbound",
		FromNumber: "+14155550100",
		ToNumber:   []string{"+14155550104"},
		Text:       "hello",
		Contact:    dialgo.Contact{ID: "ck-not-cached"},
	}
	msg := da.newDialpadMessageEvent(evt)
	if msg == nil {
		t.Fatal("expected message event, got nil")
	}
	want := "sms:+14155550100:+14155550104"
	if string(msg.portalKey.ID) != want {
		t.Errorf("portal ID = %q; want %q", msg.portalKey.ID, want)
	}
	if !msg.sender.IsFromMe {
		t.Error("outbound message should have IsFromMe=true")
	}
}

// Inbound DM with no cached contact_key composes the portal from to_phone
// (my line) and from_phone (sender).
func TestNewDialpadMessageEvent_InboundDM(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550100",
		Phones:       []string{"+14155550100"},
	}, "login-1")
	evt := &dialgo.SMSEvent{
		ID:         99,
		Direction:  "inbound",
		FromNumber: "+14155550104",
		ToNumber:   []string{"+14155550100"},
		Text:       "hi from outside",
	}
	msg := da.newDialpadMessageEvent(evt)
	if msg == nil {
		t.Fatal("expected message event, got nil")
	}
	want := "sms:+14155550100:+14155550104"
	if string(msg.portalKey.ID) != want {
		t.Errorf("portal ID = %q; want %q", msg.portalKey.ID, want)
	}
	if msg.sender.IsFromMe {
		t.Error("inbound message should have IsFromMe=false")
	}
	if string(msg.sender.Sender) != "+14155550104" {
		t.Errorf("inbound sender = %q; want +14155550104", msg.sender.Sender)
	}
}

// Locks in echo-match symmetry for MMS without caption — the bug behind the
// double-post regression. Send path now hashes(portal, clientID); echo path
// hashes the same when evt.ClientID is set. If a future edit reverts either
// side to hashing on text/filename, this test fails.
func TestNewDialpadMessageEvent_OutboundMMSTxnIDMatchesClientID(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550100",
		Phones:       []string{"+14155550100"},
	}, "login-1")

	portalID := "sms:+14155550100:+14155550104"
	clientID := "sent42"
	wantTxn := makeTextHash(portalID, clientID)

	// Captionless MMS echo: Text is empty, MMS=true, ClientID set.
	evt := &dialgo.SMSEvent{
		ID:         101,
		Direction:  "outbound",
		FromNumber: "+14155550100",
		ToNumber:   []string{"+14155550104"},
		Text:       "",
		MMS:        true,
		MediaURLs:  []string{"https://example/img.jpg"},
		ClientID:    clientID,
	}
	msg := da.newDialpadMessageEvent(evt)
	if msg == nil {
		t.Fatal("expected message event, got nil")
	}
	if msg.GetTransactionID() != wantTxn {
		t.Errorf("echo txnID = %q; want %q (hash of portal + clientID)",
			msg.GetTransactionID(), wantTxn)
	}
}

// Text echoes still get a txnID even when ClientID is missing (covers any
// legacy webhook path that doesn't carry client_id).
func TestNewDialpadMessageEvent_TextEchoFallsBackToContentHash(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550100",
		Phones:       []string{"+14155550100"},
	}, "login-1")
	portalID := "sms:+14155550100:+14155550104"
	evt := &dialgo.SMSEvent{
		ID:         102,
		Direction:  "outbound",
		FromNumber: "+14155550100",
		ToNumber:   []string{"+14155550104"},
		Text:       "hello",
		// ClientID empty — fallback to text hash.
	}
	msg := da.newDialpadMessageEvent(evt)
	if msg == nil {
		t.Fatal("expected message event, got nil")
	}
	want := makeTextHash(portalID, "hello")
	if msg.GetTransactionID() != want {
		t.Errorf("text-echo txnID = %q; want %q (hash of portal + text)",
			msg.GetTransactionID(), want)
	}
}

// Inbound messages never carry a txnID (no pending to match against).
func TestNewDialpadMessageEvent_InboundHasNoTxnID(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550100",
		Phones:       []string{"+14155550100"},
	}, "login-1")
	evt := &dialgo.SMSEvent{
		ID:         103,
		Direction:  "inbound",
		FromNumber: "+14155550104",
		ToNumber:   []string{"+14155550100"},
		Text:       "incoming",
		ClientID:    "sent99", // still ignored on inbound
	}
	msg := da.newDialpadMessageEvent(evt)
	if msg == nil {
		t.Fatal("expected message event, got nil")
	}
	if msg.GetTransactionID() != "" {
		t.Errorf("inbound txnID = %q; want empty", msg.GetTransactionID())
	}
}

// The dangerous case: an outbound group echo arrives with comma-joined
// to_phone but the contact_key cache hasn't been seeded yet (push event beat
// the sync goroutine). Previously this silently composed a corrupted DM
// portal "sms:+my:+ph1,+ph2"; now newDialpadMessageEvent must drop the
// event so the framework doesn't queue against a phantom room.
func TestNewDialpadMessageEvent_CacheMissCommaToPhoneIsDropped(t *testing.T) {
	da := newTestAPI(UserLoginMetadata{
		PrimaryPhone: "+14155550101",
		Phones:       []string{"+14155550100", "+14155550101"},
	}, "login-1")

	evt := &dialgo.SMSEvent{
		ID:         9999,
		Direction:  "outbound",
		FromNumber: "+14155550101",
		ToNumber:   []string{"+14155550103,+14155550102"}, // comma-joined group form
		Text:       "lost echo",
		Contact:    dialgo.Contact{ID: "ck-uncached"}, // cache miss
	}
	if msg := da.newDialpadMessageEvent(evt); msg != nil {
		t.Errorf("expected nil for cache-missed comma-joined to_phone, got portal %q", msg.portalKey.ID)
	}
}

func TestFeedContactIsGroup(t *testing.T) {
	if (&dialgo.FeedContact{Type: "contact_group"}).IsGroup() != true {
		t.Error("contact_group should be a group")
	}
	if (&dialgo.FeedContact{Type: ""}).IsGroup() != false {
		t.Error("empty type should not be a group")
	}
	if (&dialgo.FeedContact{Type: "contact"}).IsGroup() != false {
		t.Error("regular contact should not be a group")
	}
}
