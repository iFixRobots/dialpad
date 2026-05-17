# CLAUDE.md

Orientation for Claude Code (claude.ai/code). README.md has the user-facing setup walkthrough; this file is the agent's mental model.

## Build & Run

```bash
./build.sh                                              # produces ./dialpad-bridge
cd ~/.local/share/dialpad-bridge && dialpad-bridge      # bridge reads config.yaml from cwd

docker build -t dialpad-bridge .
./docker-run.sh                                         # uses /data inside container
```

The binary accepts `-c <path>` (default `./config.yaml`) and `-r <path>` for the registration file — there's nothing magic about the directory it runs from.

Go 1.25.0 required. The macOS `ld: warning: ignoring duplicate libraries: '-lc++', '-lolm'` is harmless.

### Optional runtime dependency: ffmpeg

The bridge uses `ffmpeg` (and `ffprobe`) on the host to transcode oversized outbound video/audio/voice-note attachments down to fit Dialpad's MMS cap. When ffmpeg is missing, those files pass through uncompressed and Dialpad rejects them server-side; image compression is unaffected (uses the Go std lib). On macOS: `brew install ffmpeg`. On Debian/Ubuntu: `apt install ffmpeg`. The bridge auto-detects ffmpeg via `$PATH` at startup and varies its advertised capabilities accordingly (audio/ogg voice notes go from `Rejected` to `PartialSupport`).

## Tests

Tests live next to the code (`*_test.go`). Run with `go test ./...`.

- `pkg/connector/`: backfill, chatsync, connector, events, handle_dialpad, handlematrix, id, media, mediacompress, mediacompress_integration
- `pkg/dialgo/`: http, internal_api

`mediacompress_integration_test.go` invokes real ffmpeg/ffprobe via lavfi-generated synthetic fixtures; it skips cleanly when those binaries aren't on `$PATH`. All other tests are pure / use stubbed interfaces.

## Architecture

Matrix bridge for Dialpad SMS/MMS/calls/voicemail, built on `maunium.net/go/mautrix` bridgev2. Module path `github.com/beeper/dialpad-bridge`.

### Two-layer design

**`pkg/dialgo/`** — standalone Dialpad protocol client, no mautrix dependency.
- Bearer-token auth (Google OAuth token extracted from the web app's `harness:session` localStorage)
- Ably push WebSocket on `wss://realtime.push.dialpad.com`, channel `UserProfile-{numeric_user_id}:main`, 1-hour tokens refreshed at 50 min, exp-backoff reconnect (max 5 min)
- Internal REST (`/api/...`) for backfill, contacts, voicemail, sending — undocumented; reverse-engineered from the web client
- `http.go` — generic JSON helpers (`getJSON[T]`, `postJSON[T]`, `patchJSON[T]`) channeled through one auth-aware `doAPIRequest`. Endpoint methods in `internal_api.go` are 4-line wrappers on top.
- Public v2 (`/api/v2`) only for a few documented endpoints (unused for send/recv currently)
- SMS rate limiting (token bucket; 100/min Tier 0, 800/min Tier 1)

**`pkg/connector/`** — bridge glue implementing `bridgev2.NetworkConnector`. Split along sibling-bridge seams (`~/gvoice`, `~/gmessages`); each file is one concern.
- `connector.go` — connector lifecycle, config, metadata types, phone helpers
- `api.go` — `DialpadAPI` struct + interface assertions + tiny shared helpers (`getMyNumber`, `seedContactKeyCache`, `portalKeyForContact`). Methods themselves live in the per-concern files below.
- `client.go` — Connect, Disconnect, IsLoggedIn, LogoutRemote, IsThisUser, registerEventHandlers
- `chatsync.go` — `syncExistingConversations`, `syncGroupConversation`, contact-name poll loop, the pure helpers (`mergeConversationsByContactKey`, `classifyDMConv`, `resolveGroupParticipants`)
- `chatinfo.go` — GetChatInfo, GetUserInfo, makeAvatar
- `capabilities.go` — `GetCapabilities`, ffmpeg-aware capability ID stamp
- `startchat.go` — ResolveIdentifier, CreateGroup, GetContactList
- `handlematrix.go` — HandleMatrixMessage, sendInternalMessage, sendMMS (image/video/audio compression dispatch), handleRemoteEcho
- `handle_dialpad.go` — inbound SMS/call/voicemail processing
- `events.go` — `RemoteMessage` impl with outbound echo matching via `client_id`-based txnID
- `backfill.go` — `FetchMessages` via internal `/api/feed/`
- `bridgestate.go` — all `BridgeErr*` constants + human-readable strings
- `id.go` — `MakeDMPortalID` / `MakeGroupPortalID` / `ParsePortalID` (centralized portal-ID composition)
- `contacts.go` — phone→contact cache
- `media.go` — MMS download + outbound recompression (images via std lib; video/audio via ffmpeg when available, with `maxMMSImageDimension=1600` carrier-safety cap) + Matrix upload
- `login.go` — `LoginProcessCookies` flow; embedded browser jumps straight to Google OAuth (`/auth/google/request?action=login`)
- `commands.go` — `!dialpad lines / set-line / clear-line / expiry / refresh-token`
- `expiry.go` — token expiry watcher + silent refresh
- `harness_session.go` — login token JSON parser

**`cmd/dialpad-bridge/`** — ~25-line entry; wires the connector into `mxmain.BridgeMain`.

## Critical concepts

These are the things an agent picking up the repo cold must know. Each gets one line.

- **Dual-scope sync** — `/api/contact/?filter=messages` is queried against BOTH the `UserProfile` and `Office` target keys and merged by `contact_key`; the office scope is where groups live.
- **Office DID merge** — at Connect, `GetOffice(officeKey)` pulls the office's DIDs into `da.meta.Phones` so `ownLines` filtering and `getMyNumber` see all of the user's lines.
- **Self-reference filter** — `/api/contact/` returns entries for the user's own lines (empty `contact_key`, phone matching `ownLines`); these must be dropped or they become phantom "Rodrz"-style portals.
- **Contact-key portal routing** — outbound group echoes carry `to_phone="+ph1,+ph2"` (comma-joined). `events.go::newDialpadMessageEvent` looks up `evt.Contact.ID` in a `contact_key → portal` cache first; the same cache routes inbound calls to existing SMS portals instead of spawning duplicates.
- **client_id echo dedup** — Dialpad reassigns `feed_key` server-side but preserves the `client_id` we send. `txnID = hash(portalID, client_id)` on both send and echo sides; without this, captionless MMS double-posts.
- **Recycle rule** — any change to sync logic, portal-ID composition, `target_key` selection, `ownLines` membership, or `newDialpadMessageEvent` routing requires a full clean recycle (`bbctl delete sh-dialpad` + DB wipe + re-login). Don't fix forward over a dirty DB.
- **Embedded-browser login** — Beeper Desktop captures the bearer token from an `Authorization` request header in the embedded browser. Login URL bypasses Dialpad's chooser and goes directly to Google OAuth. Occasionally captures a mid-OAuth header; manual paste via DevTools "Copy as cURL" is the documented fallback.

## Key conventions

- **Portal IDs.** DM = `sms:{my_number}:{their_number}` (E.164 both); group = `group:+ph1,+ph2,...` (sorted, external participants only). Always use `MakeDMPortalID` / `MakeGroupPortalID` from `pkg/connector/id.go` — do NOT compose by hand.
- **Portal metadata** stores `contact_key`, `my_number`, `target_key` (UserProfile or Office, per-conversation).
- **UserLogin metadata** stores `bearer_token`, `target_key` (UserProfile), `office_key`, `primary_phone`, `phones`, `expires_at`, cookies.
- **No public API** for: internal chat, message history/backfill, read receipts, typing indicators — all internal/undocumented.

## mautrix-go bridgev2 gotchas

- `NetworkAPI.Connect(ctx)` returns void, not error.
- `GetCapabilities()` returns `*event.RoomFeatures`.
- `LoadUserLogin` must set `login.Client = api`.
- `MatrixMessageResponse{DB: ...}` — the field is `DB`, not `DatabaseMessage`.
- `User.NewLogin()` second arg is `*database.UserLogin`, not `LoginMetadata`.
- `bridgev2.ErrInvalidLoginStep` does not exist — use `fmt.Errorf()`.

## bbctl

Self-hosted, single-user. Setup is documented in README.md §"Setup (self-hosted on Beeper hungryserv)". Clean recycle steps (assumes `$BRIDGE_DIR` is wherever you keep config + DB, e.g. `~/.local/share/dialpad-bridge`):

```bash
# 1. Server-side delete (PTY wrapper to handle the interactive prompt)
python3 /tmp/bbctl_delete.py sh-dialpad

# 2. Wipe local DB + config + registration
cd "$BRIDGE_DIR" && rm -f sh-dialpad.db* config.yaml registration.yaml

# 3. Regenerate config + register (poll past the server's 409 propagation lag)
until bbctl config --type bridgev2 -o config.yaml sh-dialpad; do sleep 2; done
bbctl register -g -o registration.yaml sh-dialpad

# 4. Restart the bridge; re-login via Beeper Desktop
dialpad-bridge >> bridge.log 2>&1 &
```

The PTY-driven `bbctl delete` wrapper at `/tmp/bbctl_delete.py` is a tiny Python script that pipes `y\n` to bbctl's confirm prompt (bbctl detects TTY and refuses stdin-piped input otherwise). If missing, the same effect comes from `printf 'y\n' | bbctl delete sh-dialpad` inside a `script` or `expect` wrapper.
