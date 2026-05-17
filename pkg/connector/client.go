package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

func (da *DialpadAPI) Connect(ctx context.Context) {
	da.log.Info().Msg("Connecting to Dialpad")

	// Detach background work from the caller's ctx (which may be
	// request-scoped). Disconnect calls da.cancel to stop everything.
	da.bgCtx, da.cancel = context.WithCancel(context.WithoutCancel(ctx))

	cfg := da.connector.Config
	if cfg.UseSandbox {
		da.client.APIBaseURL = dialgo.SandboxBaseURL
	}
	if cfg.SMSRateLimit > 0 {
		da.client.SMSRateLimiter = dialgo.NewSMSRateLimiter(cfg.SMSRateLimit)
	}

	if len(da.meta.Cookies) > 0 {
		if err := da.client.LoadCookies(da.meta.Cookies); err != nil {
			da.log.Warn().Err(err).Msg("Could not seed cookie jar from stored cookies")
		}
	}

	// Dialpad Bearer tokens have a 30-day TTL. For logins missing the
	// captured expiry, assume the token was just minted.
	if da.meta.ExpiresAt == 0 && da.meta.BearerToken != "" && len(da.meta.Cookies) > 0 {
		da.meta.ExpiresAt = time.Now().Add(30 * 24 * time.Hour).UnixMilli()
		if err := da.login.Save(ctx); err != nil {
			da.log.Warn().Err(err).Msg("Failed to persist assumed expiry")
		} else {
			da.log.Info().
				Time("expires_at", time.UnixMilli(da.meta.ExpiresAt)).
				Msg("Backfilled missing expiry with 30-day assumption")
		}
	}

	da.client.OnAuthError = func(apiErr *dialgo.APIError) {
		da.log.Warn().
			Int("status", apiErr.StatusCode).
			Str("body", apiErr.Body).
			Msg("Authentication error — attempting silent refresh")
		go func() {
			refreshCtx, cancel := context.WithTimeout(da.bgCtx, 30*time.Second)
			defer cancel()
			if err := da.attemptSilentRefresh(refreshCtx, "on-401"); err != nil {
				da.log.Warn().Err(err).Msg("Silent refresh on 401 did not succeed")
			}
		}()
	}

	if !da.client.IsLoggedIn() {
		da.log.Warn().Msg("Client is not logged in, skipping connection")
		da.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials})
		return
	}

	da.startExpiryWatcher(ctx)

	if da.meta.TargetKey == "" || da.meta.OfficeKey == "" {
		if user, err := da.client.GetCurrentUser(ctx); err != nil {
			da.log.Warn().Err(err).Msg("Failed to fetch user keys (backfill/send-from-secondary-line may not work)")
		} else {
			if da.meta.TargetKey == "" && user.Key != "" {
				da.meta.TargetKey = user.Key
			}
			if da.meta.OfficeKey == "" && user.OfficeKey != "" {
				da.meta.OfficeKey = user.OfficeKey
			}
			da.log.Info().
				Str("target_key", da.meta.TargetKey).
				Str("office_key", da.meta.OfficeKey).
				Msg("Fetched and stored internal entity keys")
			if err := da.login.Save(ctx); err != nil {
				da.log.Warn().Err(err).Msg("Failed to persist entity keys to metadata")
			}
		}
	}

	// Pull the office's phone numbers (DIDs) into our own-lines set so group
	// participant filtering excludes them. /api/contact/?target_key=<office>
	// surfaces shared office conversations but doesn't tell us which numbers
	// in those convs are "ours".
	if da.meta.OfficeKey != "" {
		if office, err := da.client.GetOffice(ctx, da.meta.OfficeKey); err != nil {
			da.log.Warn().Err(err).Msg("Failed to fetch office DIDs")
		} else {
			added := false
			existing := map[string]bool{}
			for _, p := range da.meta.Phones {
				existing[p] = true
			}
			for _, did := range office.DIDs {
				normalized := formatPhoneNumber(did)
				if normalized != "" && !existing[normalized] {
					da.meta.Phones = append(da.meta.Phones, normalized)
					existing[normalized] = true
					added = true
				}
			}
			if added {
				da.log.Info().Strs("office_dids", office.DIDs).Strs("phones", da.meta.Phones).Msg("Merged office DIDs into user phones")
				if err := da.login.Save(ctx); err != nil {
					da.log.Warn().Err(err).Msg("Failed to persist merged phones")
				}
			}
		}
	}

	da.registerEventHandlers()

	// Ensure user ID is cached (required for Ably channel subscription)
	if da.client.GetUserID() == "" {
		if _, err := da.client.GetCurrentUser(ctx); err != nil {
			da.log.Err(err).Msg("Failed to fetch user info for Ably push")
			da.login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateTransientDisconnect,
				Error:      BridgeErrUserFetchFailed,
				Message:    err.Error(),
			})
			return
		}
	}

	// Connect to Ably push for real-time events (calls, SMS, etc.)
	if err := da.client.Connect(ctx); err != nil {
		da.log.Err(err).Msg("Failed to connect to Dialpad Ably push")
		da.login.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      BridgeErrPushConnectFailed,
			Message:    err.Error(),
		})
		return
	}

	da.log.Info().Msg("Connected to Dialpad Ably push")

	// Ensure the empty-ID ghost row exists so backfill inserts of IsFromMe
	// messages (sender_id="") satisfy the message_sender_fkey constraint.
	// The framework only auto-creates this row when a sender has no double
	// puppet; double-puppeted users (Beeper) skip that path and hit FK
	// failures on the first batch.
	if _, err := da.connector.br.GetGhostByID(ctx, ""); err != nil {
		da.log.Warn().Err(err).Msg("Failed to ensure empty ghost row exists")
	}

	// Seed the contact_key → portal cache from existing portals so push events
	// arriving before the first sync completes can still find the right room.
	go da.seedContactKeyCache(da.bgCtx)

	// Sync existing conversations → create portals and trigger backfill.
	// Without this, rooms are only created for NEW messages after connect,
	// ignoring all existing conversation history.
	go da.syncExistingConversations()

	// Start periodic contact name sync.
	// Dialpad contact renames are REST-only (PATCH /api/contact/{key}) with
	// no Ably push notification. The only reliable way to detect renames is
	// to periodically poll the conversation list and compare display names.
	go da.contactNameSyncLoop()
}

func (da *DialpadAPI) Disconnect() {
	da.log.Info().Msg("Disconnecting from Dialpad")
	if da.cancel != nil {
		da.cancel()
		da.cancel = nil
	}
	if da.client != nil {
		da.client.Disconnect()
	}
}

func (da *DialpadAPI) IsLoggedIn() bool {
	return da.client != nil && da.client.IsLoggedIn()
}

// seedContactKeyCache populates contactKeyToPortal from existing portals in
// the local DB. Without this, an outbound echo arriving before sync completes
// would miss the cache and fall back to (possibly corrupted) phone-based

func (da *DialpadAPI) LogoutRemote(ctx context.Context) {
	if da.client != nil {
		_ = da.client.Logout(ctx)
	}
}

func (da *DialpadAPI) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	if da.client == nil || !da.client.IsLoggedIn() {
		return false
	}
	return string(userID) == da.client.GetUserID()
}

func (da *DialpadAPI) registerEventHandlers() {
	da.client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *dialgo.SMSEvent:
			da.handleSMSEvent(v)
		case *dialgo.CallEvent:
			da.handleCallEvent(v)
		case *dialgo.ActiveCallCheckEvent:
			go da.handleActiveCallCheck()
		case *dialgo.Connected:
			da.log.Info().Msg("Connected to Dialpad")
			da.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		case *dialgo.Disconnected:
			da.log.Warn().Str("reason", v.Reason).Msg("Disconnected from Dialpad")
			da.login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateTransientDisconnect,
				Error:      BridgeErrDisconnected,
				Message:    v.Reason,
			})
		}
	})
}
