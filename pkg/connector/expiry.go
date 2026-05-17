package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

const (
	RefreshWarningWindow = 7 * 24 * time.Hour
	RefreshAttemptWindow = 5 * 24 * time.Hour
	RefreshLoopInterval  = 30 * time.Minute
)

// Bridge-state error codes live in bridgestate.go alongside their
// human-readable strings.

func (da *DialpadAPI) FillBridgeState(state status.BridgeState) status.BridgeState {
	if da.meta == nil || da.meta.ExpiresAt == 0 {
		return state
	}
	expiresAt := time.UnixMilli(da.meta.ExpiresAt)
	remaining := time.Until(expiresAt)

	if remaining <= 0 && state.StateEvent == status.StateConnected {
		state.StateEvent = status.StateBadCredentials
		state.Error = BridgeErrTokenExpired
		state.UserAction = status.UserActionRelogin
		return state
	}
	if remaining < RefreshWarningWindow && state.StateEvent == status.StateConnected {
		state.StateEvent = status.StateTransientDisconnect
		state.Error = BridgeErrTokenExpiring
		state.UserAction = status.UserActionOpenNative
		hours := int(remaining.Hours())
		days := hours / 24
		switch {
		case days >= 1:
			state.Info = map[string]any{"expires_in": fmt.Sprintf("%d days", days)}
		case hours >= 1:
			state.Info = map[string]any{"expires_in": fmt.Sprintf("%d hours", hours)}
		default:
			state.Info = map[string]any{"expires_in": "less than an hour"}
		}
	}
	return state
}

func (da *DialpadAPI) startExpiryWatcher(ctx context.Context) {
	if da.meta == nil || da.meta.ExpiresAt == 0 {
		da.log.Debug().Msg("Skipping expiry watcher — no expiry recorded")
		return
	}
	go func() {
		ticker := time.NewTicker(RefreshLoopInterval)
		defer ticker.Stop()
		da.maybeRefresh(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				da.maybeRefresh(ctx)
			}
		}
	}()
}

func (da *DialpadAPI) maybeRefresh(ctx context.Context) {
	if da.meta == nil || da.meta.ExpiresAt == 0 {
		return
	}
	expiresAt := time.UnixMilli(da.meta.ExpiresAt)
	if time.Until(expiresAt) > RefreshAttemptWindow {
		return
	}
	da.attemptSilentRefresh(ctx, "proactive")
}

func (da *DialpadAPI) attemptSilentRefresh(ctx context.Context, trigger string) error {
	log := da.log.With().Str("trigger", trigger).Logger()
	if da.client == nil {
		return fmt.Errorf("no client")
	}
	if err := da.client.LoadCookies(da.meta.Cookies); err != nil {
		log.Warn().Err(err).Msg("Could not seed cookie jar before refresh")
	}

	res, err := da.client.RefreshBearer(ctx, da.meta.Email)
	if err != nil {
		switch {
		case errors.Is(err, dialgo.ErrRefreshNoCookies):
			log.Warn().Msg("Silent refresh impossible: no cookies stored")
			da.login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateBadCredentials,
				Error:      BridgeErrRefreshNoCookies,
				UserAction: status.UserActionRelogin,
			})
		case errors.Is(err, dialgo.ErrRefreshInteractionRequired):
			log.Warn().Msg("Google rejected silent OAuth")
			da.login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateBadCredentials,
				Error:      BridgeErrInteractionNeeded,
				UserAction: status.UserActionRelogin,
			})
		default:
			log.Err(err).Msg("Silent refresh failed")
		}
		return err
	}

	da.meta.BearerToken = res.BearerToken
	if !res.ExpiresAt.IsZero() {
		da.meta.ExpiresAt = res.ExpiresAt.UnixMilli()
	}
	if res.Email != "" {
		da.meta.Email = res.Email
	}
	da.meta.Cookies = da.client.ExportCookies()
	if err := da.login.Save(ctx); err != nil {
		log.Err(err).Msg("Failed to persist refreshed credentials")
		return err
	}
	log.Info().Time("expires_at", time.UnixMilli(da.meta.ExpiresAt)).Msg("Silent OAuth refresh succeeded")
	da.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
	return nil
}

func (da *DialpadAPI) timeUntilExpiry() time.Duration {
	if da.meta == nil || da.meta.ExpiresAt == 0 {
		return 0
	}
	return time.Until(time.UnixMilli(da.meta.ExpiresAt))
}
