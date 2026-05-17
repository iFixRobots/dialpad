package connector

import "maunium.net/go/mautrix/bridgev2/status"

// Bridge-state error codes the connector emits via BridgeState.Send. Codes
// are part of the public surface — Beeper Desktop renders them via the
// BridgeStateHumanErrors map registered below, and external tooling may
// also depend on the exact string values. Don't rename in place; add new
// codes and migrate uses first.
const (
	// Token-expiry / silent-refresh family. See expiry.go for the state
	// transitions that fire these.
	BridgeErrTokenExpiring     status.BridgeStateErrorCode = "dialpad-token-expiring"
	BridgeErrTokenExpired      status.BridgeStateErrorCode = "dialpad-token-expired"
	BridgeErrRefreshNoCookies  status.BridgeStateErrorCode = "dialpad-refresh-no-cookies"
	BridgeErrInteractionNeeded status.BridgeStateErrorCode = "dialpad-interactive-reauth"

	// Connect / runtime family. Fired from client.go.
	BridgeErrUserFetchFailed  status.BridgeStateErrorCode = "dialpad-user-fetch-failed"
	BridgeErrPushConnectFailed status.BridgeStateErrorCode = "dialpad-push-connect-failed"
	BridgeErrDisconnected     status.BridgeStateErrorCode = "dialpad-disconnected"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		BridgeErrTokenExpiring: "Your Dialpad session is about to expire. " +
			"The bridge will try to refresh it automatically. If that fails, sign in again at dialpad.com.",
		BridgeErrTokenExpired: "Your Dialpad session has expired. " +
			"Sign in to Dialpad again to reconnect — use 'Sign in with Google' so the bridge can keep refreshing the session for you.",
		BridgeErrRefreshNoCookies: "This login was created before silent refresh was supported. " +
			"Sign in to Dialpad again to enable automatic session refresh.",
		BridgeErrInteractionNeeded: "Google asked for an interactive sign-in (likely because of an MFA challenge or session change). " +
			"Sign in to Dialpad again in Beeper to resume.",
		BridgeErrUserFetchFailed: "Couldn't fetch the Dialpad user profile for the realtime channel. " +
			"The bridge will retry automatically; if it keeps failing, your token may have expired.",
		BridgeErrPushConnectFailed: "Couldn't connect to Dialpad's push channel. " +
			"The bridge will retry automatically.",
		BridgeErrDisconnected: "Disconnected from Dialpad's push channel. " +
			"The bridge will reconnect automatically.",
	})
}
