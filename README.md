# Dialpad for Beeper

A self-hosted connection that brings Dialpad SMS, MMS, calls, and voicemail into Beeper Desktop. The bridge runs on your machine and syncs Dialpad conversations into your Beeper account.

## What works

- ✅ SMS send and receive
- ✅ MMS send and receive
- ✅ Voicemail (delivered as native voice notes)
- ✅ Call notices (missed, incoming, outgoing)
- ✅ Message history backfill
- ✅ Contact name sync
- ✅ Message send failure reporting
- ✅ Contact list for Beeper's contact picker
- ✅ Multi-number routing — separate Beeper chat per `(your_line, their_number)` pair on receive and reply
- ✅ Silent session refresh attempt — the bridge tries to renew your Dialpad session before its roughly 30-day expiry when the captured cookies allow it

## What doesn't work yet

- Starting brand-new chats (DMs or group SMS) from inside Beeper. Receiving and replying to existing chats works fine. Start new conversations from the Dialpad app or website; the connection picks them up automatically.

## Audience

This README is written for anyone with an AI coding assistant (Claude, Cursor, Codex, etc.) and a Beeper account. The only thing assumed is that Beeper Desktop is installed. Everything else — `bbctl`, the connection binary, the runtime directory — the walkthrough below sets up for you.

## For an AI agent helping a user set this up

If you (the AI) are sitting next to a user who wants this running, use the script below as a starting point. The user may not be a developer. Speak plainly. Keep their hands on the keyboard only when they have to be.

**Open with this (paraphrased, not copy-pasted):**
> "Heads-up before we start: this connects your Dialpad SMS, calls, and voicemail to Beeper. Once we set it up, your Dialpad conversations show up inside Beeper Desktop like any other chat, and replies you send go out on your Dialpad number. Setup takes about 5 minutes and we only do it once."
>
> "I'll need you to do three things during setup: (1) approve a couple of commands I'll run for you, (2) sign in to Dialpad with Google when a window pops up, and (3) confirm everything looks right at the end. I'll handle the rest."

> **Important disclaimer up front, tell the user:** *"Beeper Desktop's **Settings → Bridges** menu only appears once a self-hosted connection actually exists on your account. Right now it won't be visible — that's expected. The menu shows up automatically after I finish the setup steps. If you go looking for it before we're done, don't panic that it's missing."*

**Pre-flight check (do this silently before asking the user for anything):**
- Confirm Beeper Desktop is installed (`ls /Applications/Beeper*.app` on macOS, or the equivalent on Linux). If not, tell the user: *"Install Beeper Desktop from https://www.beeper.com first, sign in, and let me know when you're done."*
- Confirm `bbctl` is on PATH: `which bbctl`. If not, install it (see step 1 below).
- Detect the user's OS + CPU so you grab the right binary:
  - macOS Apple Silicon → `darwin-arm64`
  - Linux x86_64 → `linux-amd64`
  - Linux ARM64 (e.g. Raspberry Pi 4/5 64-bit) → `linux-arm64`

**Setup script (what the AI does, with what the AI says alongside):**

1. **Install `bbctl` if missing.** `bbctl` is Beeper's self-hosting CLI ([github.com/beeper/bridge-manager](https://github.com/beeper/bridge-manager)). Grab the right binary for the user's OS/CPU and put it on PATH:
   ```sh
   curl -L -o /usr/local/bin/bbctl https://github.com/beeper/bridge-manager/releases/latest/download/bbctl-<os-arch>
   chmod +x /usr/local/bin/bbctl
   ```
   Then log them into their Beeper account: `bbctl login` (interactive — opens a browser).
   Say: *"Installing Beeper's self-hosting CLI and signing you in. A browser window will pop up — finish the Beeper login there and come back."*

2. **Create the runtime directory.** Run: `mkdir -p ~/.local/share/dialpad-bridge && cd ~/.local/share/dialpad-bridge`.
   Say: *"Setting up a folder under your home directory for the connection's config and local database."*

3. **Register the connection on the user's Beeper account and generate its config.** Run:
   ```sh
   bbctl config --type bridgev2 -o config.yaml sh-dialpad
   bbctl register -g -o registration.yaml sh-dialpad
   ```
   (If the server-side propagation lag returns HTTP 409 on the first call, retry every few seconds until it succeeds.)
   Say: *"Registering this connection on your Beeper account. Beeper now knows it exists and how to talk to it."*

4. **Download the connection binary.** Grab the asset matching the user's OS/CPU from [releases/latest](https://github.com/iFixRobots/dialpad/releases/latest):
   ```sh
   curl -L -o dialpad-bridge https://github.com/iFixRobots/dialpad/releases/latest/download/dialpad-bridge-<os-arch>
   chmod +x dialpad-bridge
   ```
   Say: *"Downloading — about a 30 MB file."*

5. **Start it in the background.** Run: `./dialpad-bridge >> bridge.log 2>&1 &`.
   Confirm it's alive: `bbctl whoami | grep dialpad`. Expect `sh-dialpad (bridgev2, self-hosted) - RUNNING`.
   Say: *"Started. Beeper Desktop should pick it up in a few seconds."*

6. **Hand off to the user for sign-in.** This is where they take over:
   > "Open Beeper Desktop. Go to **Settings → Bridges**. (This menu just appeared because the connection is now running on your account — it wasn't there before.) Scroll to the **Self-hosted Bridges** section — you should see `sh-dialpad`. Right-click it and choose **Experimental: Add an account**. A window pops up at the Dialpad login page — click **Sign in with Google** and complete the flow normally. Don't paste anything manually. The window closes and your Dialpad conversations start appearing in Beeper within a few seconds."

7. **Verify after sign-in.** Check `bridge.log` for `Dialpad login successful`. The line should show `has_dialpad_cookie=true` and `has_google_cookies=true`.
   - If `has_google_cookies=false`: tell the user *"Something went wrong capturing Google cookies — silent refresh won't work reliably. You can re-do the sign-in later to fix this."*
   - Otherwise: *"You're set. The connection will try to refresh your Dialpad session in the background. If Google or Dialpad requires a fresh interactive sign-in, Beeper will ask you to log in again."*

**What the AI should explain about features (in plain English, only if the user asks):**
- **Texting**: *"Your Dialpad conversations show up in Beeper just like any other chat. Replies go out from your Dialpad number. Pictures work both directions."*
- **Voicemails**: *"When someone leaves you a voicemail, it shows up in Beeper as a voice note you can play right there."*
- **Calls**: *"You'll see incoming, outgoing, and missed calls as notices in the chat. The connection doesn't make calls — you still place those in Dialpad itself."*
- **Multiple Dialpad numbers**: *"Each conversation lives in its own Beeper chat based on which of your numbers the other person texted."*
- **Re-login**: *"The connection tries to refresh your Dialpad session in the background. If Google or Dialpad requires a fresh security check, Beeper will ask you to sign in again."*

**What to say when things break:**
- *"Sign in failed with 'Couldn't sign you in'"* → restart Beeper Desktop and try again. If it persists, the embedded sign-in window may need a binary refresh — re-download the latest release.
- *"`UNKNOWN_TOKEN` errors at startup"* → Beeper rotated the appservice tokens server-side. Re-run step 3 to regenerate `config.yaml`.
- *"Messages aren't syncing"* → run `bbctl whoami | grep dialpad`. `RUNNING - remote: CONNECTED` = healthy. `BAD_CREDENTIALS` = user needs to re-do the sign-in.
- *"Connection shows offline in Beeper"* → check the process is alive (`ps aux | grep dialpad-bridge`). If it died, restart it from the runtime dir. If it keeps dying, check `bridge.log` for the error.

**What the AI should NEVER do without asking the user:**
- Run `bbctl delete sh-dialpad` (destructive — wipes server-side state and all the synced chats).
- Push commits or open PRs.
- Modify `config.yaml` outside the credential fields without explanation.
- Modify `~/.bbctl/*` or anything in the user's Beeper Desktop install.

## Manual setup (without the AI walkthrough)

If you're setting this up yourself without an AI agent, the same steps abbreviated:

```bash
# 1. Install bbctl if you don't have it
#    https://github.com/beeper/bridge-manager — grab a release binary
bbctl login   # signs you into your Beeper account

# 2. Pick a runtime directory and create the config there
export BRIDGE_DIR=~/.local/share/dialpad-bridge
mkdir -p "$BRIDGE_DIR" && cd "$BRIDGE_DIR"
bbctl config --type bridgev2 -o config.yaml sh-dialpad
bbctl register -g -o registration.yaml sh-dialpad

# 3. Download the prebuilt binary for your OS/CPU. Replace <os-arch>
#    with one of: darwin-arm64, linux-amd64, linux-arm64.
curl -L -o dialpad-bridge \
  https://github.com/iFixRobots/dialpad/releases/latest/download/dialpad-bridge-<os-arch>
chmod +x dialpad-bridge

# 4. Run it. Reads ./config.yaml by default.
./dialpad-bridge >> bridge.log 2>&1 &
```

Verify:

```bash
bbctl whoami | grep dialpad
# Expected:
# sh-dialpad (bridgev2, self-hosted) - RUNNING - remote: CONNECTED (...)
```

Then in Beeper Desktop: **Settings → Bridges → Self-hosted Bridges → right-click `sh-dialpad` → Experimental: Add an account**.

Reset cleanly with `bbctl delete sh-dialpad` (interactive — needs `y` to confirm), then start over.

## Signing in to Dialpad

1. Open **Beeper Desktop** → **Settings** → **Bridges** (this menu only appears once `sh-dialpad` is registered on your account).
2. Scroll to **Self-hosted Bridges** — `sh-dialpad` should be listed.
3. Right-click `sh-dialpad` → **Experimental: Add an account**.
4. An embedded browser opens at the Dialpad login page.
5. Click **"Sign in with Google"** and complete the flow — pick your account, finish 2FA if Google asks for it. Don't paste anything manually.
6. The window closes and your conversations start syncing within a few seconds.

`bridge.log` should show `Dialpad login successful` with `has_dialpad_cookie=true` and `has_google_cookies=true`. If `has_google_cookies=false`, automatic refresh may not work and you'll need to re-sign in when the Dialpad session expires.

## Session lifetime & auto-refresh

Dialpad sessions are roughly 30 days, and the bridge stores the expiry when Dialpad provides it. The connection attempts a silent refresh before the stored session expires. If Google or Dialpad requires an interactive sign-in, you'll see a re-login prompt in Beeper. Re-clicking "Experimental: Add an account" is usually a quick reauth.

## Multi-number behavior

Receiving on multiple Dialpad lines and replying on the right line both work without configuration — the line is baked into each chat. Each `(your_line, their_number)` pair gets its own Beeper chat.

Starting a new chat from Beeper uses your primary line by default. To change the default for new chats only:

```
!dialpad lines                    # list available lines + current default
!dialpad set-line +14155551234   # set the default
!dialpad clear-line              # revert to primary
```

Existing chats keep their per-chat line regardless. The preference persists across restarts.

## Layout

```
cmd/dialpad-bridge/main.go          Entry point (~25 lines)
pkg/connector/                      Connector glue
  connector.go                        Lifecycle, config schema, metadata, phone helpers
  api.go                              DialpadAPI struct + interface assertions + tiny shared helpers
  client.go                           Connect / Disconnect / IsLoggedIn / event handler registration
  chatsync.go                         syncExistingConversations, group sync, contact-name poll loop
  chatinfo.go                         GetChatInfo, GetUserInfo, avatar handling
  capabilities.go                     GetCapabilities + ffmpeg-aware capability ID stamp
  startchat.go                        ResolveIdentifier, CreateGroup, GetContactList
  handlematrix.go                    HandleMatrixMessage, sendInternalMessage, sendMMS (image/video/audio dispatch)
  handle_dialpad.go                   Inbound SMS / MMS / call / voicemail handlers
  events.go                           RemoteMessage impl + echo matching
  backfill.go                         Message + call history pagination and conversion
  bridgestate.go                      Error-code constants + human-readable strings
  id.go                               Chat-ID composition / parsing
  contacts.go                         Thread-safe phone→contact cache
  media.go                            MMS upload/download + outbound recompression (image std-lib; video/audio via ffmpeg)
  expiry.go                           Session-expiry watcher + silent refresh trigger
  login.go                            Embedded-browser login
  harness_session.go                  Login token parser
  commands.go                         !dialpad lines / set-line / clear-line / expiry / refresh-token
pkg/dialgo/                         Standalone Dialpad client (no mautrix import)
  client.go, auth.go                  HTTP plumbing
  http.go                             Generic JSON helpers
  cookies.go                          Cookie persistence
  auth_refresh.go                     Silent session refresh
  websocket.go                        Realtime push WebSocket
  events.go, call_events.go           Event types + dispatch
  delta_events.go                     Presence/profile patches
  internal_api.go                     REST call wrappers
  groups.go                           Group management
  send.go                             Public SMS API client (unused — kept for parity)
  ratelimit.go                        Token bucket for SMS rate limiting
  download.go                         Media download
  errors.go                           API error wrapper
  types/message.go, contact.go        Shared structs
$BRIDGE_DIR/                        Runtime (your choice of path; e.g. ~/.local/share/dialpad-bridge):
                                      config.yaml, registration.yaml, sh-dialpad.db, bridge.log
```

## Build & development

```bash
# Build with the embedded version metadata (git tag + commit + build time):
./build.sh

# Or plain go build:
go build -o dialpad-bridge ./cmd/dialpad-bridge

# Run against the Dialpad sandbox API (in your config.yaml under network:)
#   use_sandbox: true
```

Go 1.25+ is required. Tests live next to the code (`*_test.go`); run with `go test ./...`. The `ld: warning: ignoring duplicate libraries: '-lc++', '-lolm'` linker warning on macOS is harmless.

**Optional: `ffmpeg` on the host.** When `ffmpeg` and `ffprobe` are in `$PATH`, oversized outbound video / audio / voice notes get transcoded down to fit Dialpad's 2 MiB MMS cap (H.264 baseline 360p for video; AAC for audio). Without ffmpeg, large video/audio gets sent as-is and Dialpad rejects it. Image compression works either way (Go std lib). Install with `brew install ffmpeg` (macOS) or `apt install ffmpeg` (Debian/Ubuntu).

Key dependencies:

- `maunium.net/go/mautrix` v0.26.4 (bridgev2 framework)
- `github.com/coder/websocket` (WebSocket client)
- `github.com/nyaruka/phonenumbers` (E.164 normalization)
- `github.com/rs/zerolog` (logging)
- `ffmpeg` + `ffprobe` (optional, runtime — for non-image media compression)

## Operational notes

- **`conn_replaced` + `UNKNOWN_TOKEN` on restart** are normal when Beeper Desktop re-attaches the connection (e.g. after deleting + re-adding to apply code changes). Re-run `bbctl config --type bridgev2` to refresh tokens.
- **Logs**: write to wherever you redirected stdout (`bridge.log` in the examples above).
- **Management chat**: `@sh-dialpadbot:beeper.local` is the management DM. Send `help` to see commands.

## License

Licensed under the GNU Affero General Public License v3.0 — see `LICENSE`.
