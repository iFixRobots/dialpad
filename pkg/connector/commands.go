package connector

import (
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/commands"
)

var dialpadHelpSection = commands.HelpSection{
	Name:  "Dialpad",
	Order: 50,
}

var cmdLines = &commands.FullHandler{
	Func: fnLines,
	Name: "lines",
	Help: commands.HelpMeta{
		Section:     dialpadHelpSection,
		Description: "List your Dialpad phone lines and the current default for new chats.",
	},
	RequiresLogin: true,
}

func fnLines(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No Dialpad login found.")
		return
	}
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok {
		ce.Reply("Login metadata is the wrong type — please re-login.")
		return
	}
	if len(meta.Phones) == 0 {
		ce.Reply("No Dialpad lines are stored on your login. Try re-logging in.")
		return
	}

	defaultLine := meta.PreferredLine
	source := "set via `!dialpad set-line`"
	if defaultLine == "" {
		defaultLine = meta.PrimaryPhone
		source = "your account's primary line"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Your Dialpad lines:\n")
	for _, p := range meta.Phones {
		marker := ""
		if p == defaultLine {
			marker = fmt.Sprintf("  ← default for new chats (%s)", source)
		}
		fmt.Fprintf(&b, "- `%s` (%s)%s\n", p, formatDisplayPhone(p), marker)
	}
	if meta.PreferredLine != "" {
		fmt.Fprintf(&b, "\nUse `!dialpad clear-line` to revert to the primary line.")
	} else if len(meta.Phones) > 1 {
		fmt.Fprintf(&b, "\nUse `!dialpad set-line +1...` to send new chats from a different line.")
	}
	ce.Reply(b.String())
}

var cmdSetLine = &commands.FullHandler{
	Func: fnSetLine,
	Name: "set-line",
	Help: commands.HelpMeta{
		Section:     dialpadHelpSection,
		Description: "Set the outbound line for new chats started from Beeper (multi-line accounts only). Use `!dialpad lines` to see available lines.",
		Args:        "<+E164 number>",
	},
	RequiresLogin: true,
}

func fnSetLine(ce *commands.Event) {
	if len(ce.Args) != 1 {
		ce.Reply("Usage: `!dialpad set-line +14155551234`. Run `!dialpad lines` first to see what's available.")
		return
	}
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No Dialpad login found.")
		return
	}
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok {
		ce.Reply("Login metadata is the wrong type — please re-login.")
		return
	}
	requested := formatPhoneNumber(ce.Args[0])
	if requested == "" {
		ce.Reply("Couldn't parse %q as a phone number. Use E.164 form like `+14155551234`.", ce.Args[0])
		return
	}
	found := false
	for _, p := range meta.Phones {
		if p == requested {
			found = true
			break
		}
	}
	if !found {
		ce.Reply("`%s` isn't one of your Dialpad lines. Run `!dialpad lines` to see what's available.", requested)
		return
	}
	if meta.PreferredLine == requested {
		ce.Reply("`%s` is already the default for new chats.", requested)
		return
	}
	meta.PreferredLine = requested
	if err := login.Save(ce.Ctx); err != nil {
		ce.Reply("Saved the preference in memory but couldn't persist it: %v", err)
		return
	}
	ce.Reply("New chats from Beeper will now go out from `%s` (%s).", requested, formatDisplayPhone(requested))
}

var cmdClearLine = &commands.FullHandler{
	Func: fnClearLine,
	Name: "clear-line",
	Help: commands.HelpMeta{
		Section:     dialpadHelpSection,
		Description: "Clear the preferred outbound line so new chats use your Dialpad account's primary number again.",
	},
	RequiresLogin: true,
}

func fnClearLine(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No Dialpad login found.")
		return
	}
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok {
		ce.Reply("Login metadata is the wrong type — please re-login.")
		return
	}
	if meta.PreferredLine == "" {
		ce.Reply("No preferred line is set — new chats already use the primary line (`%s`).", meta.PrimaryPhone)
		return
	}
	previous := meta.PreferredLine
	meta.PreferredLine = ""
	if err := login.Save(ce.Ctx); err != nil {
		ce.Reply("Cleared the preference in memory but couldn't persist it: %v", err)
		return
	}
	ce.Reply("Cleared `%s`. New chats from Beeper will now use the primary line (`%s`).", previous, meta.PrimaryPhone)
}

var cmdExpiry = &commands.FullHandler{
	Func: fnExpiry,
	Name: "expiry",
	Help: commands.HelpMeta{
		Section:     dialpadHelpSection,
		Description: "Show when the current Dialpad session token expires.",
	},
	RequiresLogin: true,
}

func fnExpiry(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No Dialpad login found.")
		return
	}
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok {
		ce.Reply("Login metadata is the wrong type — please re-login.")
		return
	}
	if meta.ExpiresAt == 0 {
		ce.Reply("No expiry recorded on this login. Silent refresh is disabled until you re-login via the Google flow.")
		return
	}
	expires := time.UnixMilli(meta.ExpiresAt)
	remaining := time.Until(expires).Round(time.Minute)
	ce.Reply("Token expires at `%s` (in %s). Refresh attempts begin within %s of expiry.",
		expires.Format(time.RFC3339), remaining, RefreshAttemptWindow)
}

var cmdRefreshToken = &commands.FullHandler{
	Func: fnRefreshToken,
	Name: "refresh-token",
	Help: commands.HelpMeta{
		Section:     dialpadHelpSection,
		Description: "Force a silent token refresh now.",
	},
	RequiresLogin: true,
}

func fnRefreshToken(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No Dialpad login found.")
		return
	}
	api, ok := login.Client.(*DialpadAPI)
	if !ok || api == nil {
		ce.Reply("Dialpad client not ready — try again after the bridge has connected.")
		return
	}
	if api.client == nil {
		ce.Reply("Dialpad HTTP client not initialised.")
		return
	}
	before := api.meta.ExpiresAt
	if err := api.attemptSilentRefresh(ce.Ctx, "manual"); err != nil {
		ce.Reply("Silent refresh failed: %v", err)
		return
	}
	after := api.meta.ExpiresAt
	if after == 0 {
		ce.Reply("Refresh returned without an expiry — the new token may be valid but its TTL is unknown.")
		return
	}
	ce.Reply("Silent refresh succeeded. New expiry: `%s` (was: `%s`).",
		time.UnixMilli(after).Format(time.RFC3339),
		formatExpiryTimestamp(before))
}

func formatExpiryTimestamp(ms int64) string {
	if ms == 0 {
		return "unset"
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}
