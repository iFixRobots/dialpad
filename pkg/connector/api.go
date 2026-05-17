package connector

import (
	"context"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// DialpadAPI implements bridgev2.NetworkAPI — one instance per logged-in user.
//
// Receiver methods are split across files by concern, mirroring sibling
// bridges (~/gmessages, ~/gvoice). The struct + interface assertions + the
// smallest cross-cutting helpers (getMyNumber, portalKeyForContact,
// seedContactKeyCache) live here; everything else is in client.go,
// chatsync.go, chatinfo.go, capabilities.go, startchat.go, handlematrix.go.
type DialpadAPI struct {
	connector *DialpadConnector
	login     *bridgev2.UserLogin
	meta      *UserLoginMetadata
	log       zerolog.Logger

	client        *dialgo.Client
	activeCallIDs sync.Map      // tracks call IDs we've already dispatched ringing for (dedup)
	contacts      *contactCache // cached contacts for O(1) ghost lookups

	// bgCtx is the lifetime ctx for all background goroutines launched from
	// Connect (sync loop, contact-name poll, on-401 refresh). It's detached
	// from the caller's ctx via WithoutCancel so a short-lived caller ctx
	// doesn't kill the bridge — Disconnect calls cancel to stop everything.
	bgCtx  context.Context
	cancel context.CancelFunc

	// contactKeyToPortal maps a Dialpad contact_key (DM contact or contact_group)
	// to the canonical portal key. Populated at sync time and at portal-create
	// time so incoming SMS push events can be routed by contact_key rather than
	// reconstructing portal IDs from to_phone (which is comma-joined for groups).
	contactKeyToPortal sync.Map // key: contact_key string, value: networkid.PortalKey
}

var (
	_ bridgev2.NetworkAPI                    = (*DialpadAPI)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*DialpadAPI)(nil)
)

// getMyNumber returns hint if set, else PreferredLine, else PrimaryPhone, else first of Phones.
func (da *DialpadAPI) getMyNumber(hint string) string {
	if hint != "" {
		return hint
	}
	if da.meta.PreferredLine != "" {
		return da.meta.PreferredLine
	}
	if da.meta.PrimaryPhone != "" {
		return da.meta.PrimaryPhone
	}
	if len(da.meta.Phones) > 0 {
		return da.meta.Phones[0]
	}
	return ""
}

func (da *DialpadAPI) newClient() {
	if da.meta.BearerToken != "" {
		da.client = dialgo.NewClient(nil, da.log)
		da.client.SetBearerToken(da.meta.BearerToken)
	}
}

// seedContactKeyCache populates contactKeyToPortal from existing portals in
// the local DB. Without this, an outbound echo arriving before sync completes
// would miss the cache and fall back to (possibly corrupted) phone-based
// composition.
func (da *DialpadAPI) seedContactKeyCache(ctx context.Context) {
	portals, err := da.connector.br.GetAllPortals(ctx)
	if err != nil {
		da.log.Warn().Err(err).Msg("Failed to seed contact_key cache from existing portals")
		return
	}
	count := 0
	for _, p := range portals {
		if p.Receiver != da.login.ID {
			continue
		}
		meta, ok := p.Metadata.(*PortalMetadata)
		if !ok || meta == nil || meta.ContactKey == "" {
			continue
		}
		da.contactKeyToPortal.Store(meta.ContactKey, p.PortalKey)
		count++
	}
	da.log.Debug().Int("count", count).Msg("Seeded contact_key → portal cache")
}

// portalKeyForContact returns the portal key cached for a given Dialpad
// contact_key, or false if unknown. Used by inbound push event handling to
// route by contact_key (covers both DMs and contact_groups, and doesn't break
// when to_phone is the comma-joined group form).
func (da *DialpadAPI) portalKeyForContact(contactKey string) (networkid.PortalKey, bool) {
	if contactKey == "" {
		return networkid.PortalKey{}, false
	}
	v, ok := da.contactKeyToPortal.Load(contactKey)
	if !ok {
		return networkid.PortalKey{}, false
	}
	return v.(networkid.PortalKey), true
}
