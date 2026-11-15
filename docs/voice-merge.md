# Merging video into voice channels

Goal: in the boson client, a voice channel gets **camera** and **screen share** buttons; turning one on shares into a video room that *is* that voice channel. Voice (quicvoiced) stays the authority on who is in which channel; video (quicvideod) only checks signatures and follows voice's word on who has to leave. quicvideod can run on the main box (stack `video` profile) or as a remote node.

Status (2026-11-15):

| Part | Where | State |
|---|---|---|
| quicvideod: multi-JWKS, control API, `Sync` | boson-chat/quicvideo#11, JWKS refresh fixes #12, #13 | merged, released **1.7.2** |
| quicvoiced: tickets, JWKS, control link, `client.VideoTicket` | boson-chat/quicvoice#9 | open for review (not merged) |
| stack: `video` profile, remote node, CI smoke | boson-chat/stack#22 | open for review (not merged) |
| boson desktop client | plan below | not started |

Nothing is deployed.

## Flow

```
client ──(voice control stream)── quicvoiced                     quicvideod
  │  VideoTicketRequest ──────────►  in a channel? sign ticket
  │  ◄──────────────────────────── VideoTicket{token, urls, exp}
  │
  │  Hello{token=ticket} ─────────────────────────────────────────► verify (voice JWKS)
  │  Join{room=<voice channel id>} ─────────────────────────────────► rooms claim allows it
  │
  │                       quicvoiced ──POST /v1/control/events────► kick from video room
  │                       (leave, kick, channel closed; every 30 s a full snapshot)
```

## 1. Video tickets (quicvoiced)

- quicvoiced holds an **Ed25519 key** (`VIDEO_TICKET_KEY_FILE`, default `/data/video-ticket.key` in the voice data volume, created 0600 on first start) and publishes the public key as a **JWKS** at `GET /.well-known/video-jwks.json` on its public listener (`https://voice.<domain>/...`, already routed by Traefik). `kid` = first 16 hex chars of SHA-256(public key), so a rotated key gets a new kid and old tickets verify until they expire.
- New control message **`VideoTicketRequest{}`** → **`VideoTicket{token, expires_at, quic_url, webtransport_url, room}`**. Only for a session that is **in a voice channel** (`Error{FORBIDDEN, "join a voice channel first"}`), and only with `VIDEO_QUIC_URL` set (`"video isn't enabled on this network"`).
- Ticket = EdDSA JWT:
  - `iss` = `VIDEO_TICKET_ISSUER` (default `voice`), `aud` = `quicvideo`, `exp` = now + `VIDEO_TICKET_TTL` (5 min);
  - `sub` = the voice identity's user id, `name` = display name;
  - `rooms` = **[the voice channel id]**: the video room *is* the voice channel;
  - `perms` = `["video.publish"]` if the member can speak in the channel, `[]` for listeners (watching is always allowed).
- The client asks for a new ticket before `expires_at` and sends it to quicvideod as `Refresh` (already supported). A client that left the voice channel can't get a new ticket, so at worst it keeps video for the rest of a 5-minute ticket; the control link below closes that gap at once.

Why tickets from quicvoiced instead of tokens from auth: voice already knows channel membership, locks, roles and mutes; the video node needs no access to auth's internal API or Ergo, only voice's public JWKS. Standalone quicvideod keeps accepting auth's tokens (`AUTH_JWKS_URL`), so both work side by side.

## 2. Verifying tickets (quicvideod)

- `AUTH_JWKS_URL` and `AUTH_ISSUER` take comma-separated lists: e.g. auth's JWKS + voice's video JWKS. Keys are matched by `kid`; issuers must be one of the configured ones; the audience stays `quicvideo`.
- The `rooms` claim already limits Join; `perms` already decides publishing.
- Unknown `kid` (quicvideod booted before quicvoiced, or a new key): a refresh at most every 200 ms, and `Verify` retries such a token for up to 3 s, so tickets arriving together all get in (#12, #13; jwkset bounds the refresh with the same context as the limiter wait, which a 1 ms budget broke across a container network).

## 3. Control link: voice → video

quicvoiced pushes membership changes to quicvideod over HTTPS (outbound from voice's side; quicvideod's control listener is the only thing it needs to reach):

- quicvideod: **`CONTROL_HTTP_ADDR`** (default off; `:8091` in the stack), bearer **`CONTROL_TOKEN`**.
  - `POST /v1/control/events` `{"events":[{"type":"leave","room":"general","user":"u1"},{"type":"close","room":"general"}]}`: `leave` removes that user's sessions from the video room (`Error{KICKED}` "left the voice channel"), `close` empties the room.
  - `PUT /v1/control/rooms` `{"rooms":{"general":["u1","u2"]}}`: the full membership snapshot. Anyone in a video room who isn't in the voice channel is removed, except sessions that joined in the last 15 s (they may have joined voice after the snapshot was taken). With `VOICE_SYNC=true`, rooms missing from the snapshot are emptied too.
- quicvoiced: **`VIDEO_CONTROL_URL`** + **`VIDEO_CONTROL_TOKEN`**. The room service reports a leave when a user's *last* session leaves a channel (self, disconnect, kick), and closes (channel deleted); a worker batches them and POSTs with backoff, and PUTs a snapshot every 30 s so a missed event heals.
- The snapshot only lists channels with someone in them, so a quicvideod whose rooms are all voice channels runs with `VOICE_SYNC=true` (the stack does).
- Known gap: someone who left voice still holds a ticket (≤ 5 min). Leaving kicks them at once, but they could rejoin video with it until the next snapshot removes them (≤ 30 s + 15 s grace). Closing it fully means quicvideod remembering recent leaves per room; not done.

## 4. Stack (`video` profile)

- `quicvideod` service from `harbor.floret.dev/boson/quicvideo:1.7.2`, UDP 7710 published, `VIDEO_HOST` added to `CERT_HOSTS` (certsync copies the cert), Traefik routes `https://VIDEO_HOST` to its public listener (`/healthz`, `/v1/info`).
- Env: `AUTH_JWKS_URL=http://quicvoiced:8090/.well-known/video-jwks.json`, `AUTH_ISSUER=voice` (only voice tickets: video in the stack is always a voice channel's), `CONTROL_HTTP_ADDR=:8091` (compose network only), `CONTROL_TOKEN=${VIDEO_CONTROL_TOKEN}`, `VOICE_SYNC=true`.
- quicvoiced: `VIDEO_HOST` set turns it on, defaulting `VIDEO_QUIC_URL=quic://VIDEO_HOST:7710`, `VIDEO_WEBTRANSPORT_URL=https://VIDEO_HOST:7710/wt`, `VIDEO_CONTROL_URL=http://quicvideod:8091`; the key is `/data/video-ticket.key`. Unset = no change for existing installs.
- **Remote node**: `compose.video-node.yaml` runs quicvideod (+ Traefik for its certificate) on another host. It reaches voice's JWKS over public HTTPS (`https://VOICE_HOST/.well-known/video-jwks.json`), and quicvoiced reaches its control listener at `https://VIDEO_HOST/control/...` through that host's Traefik (bearer token). Nothing else crosses hosts.
- `scripts/setup.sh` asks for `VIDEO_HOST` with the `video` profile and generates `VIDEO_CONTROL_TOKEN`. CI smoke runs the profile.

## Tested so far

- Unit and QUIC tests in each repo (ticket sign/verify against the served JWKS, claims, key file, link retries and snapshots, room observer with two devices, control API, `Sync`, JWKS down at boot + concurrent first tickets).
- Real quicvoiced + quicvideod, as binaries and as containers with the stack's env (quicvideod started first): two people in voice `general` get tickets; one publishes camera, the other receives 30 fps / 1.5 Mbps; a ticket for another room → `FORBIDDEN`; a foreign token → `UNAUTHENTICATED`; the control API without the token → 401; leaving voice → `KICKED left the voice channel` and their video stops; after a quicvideod restart a stale member is removed by the next snapshot (`KICKED not in the voice channel`).
- Not yet: a full stack bring-up with real auth tokens (CI smoke covers the video profile's routing and certs, and the JWKS once quicvoice 1.8 is the image).

## 5. Client (boson desktop): plan

Not implemented yet. What boson has today (main, 0.57.1):
- **Voice is desktop-only.** `desktop/voice.go` holds `VoiceService` on quicvoice's Go client (`client.Dial`, `Join`, `OnControl`), with audio in Go/cgo (miniaudio, static libopus).
- The UI (Preact, in a Wails v3 webview) reaches voice through `window.bosonVoice` (`modules/voice/voice.bridge.ts`), which is absent in browsers and on iOS/Android.
- **Webviews:** WKWebView on macOS, WebView2 on Windows, WebKitGTK 6.0 on Linux.
- **Builds:** desktop builds are per-OS and native, with cgo, in `desktop.yml`.

### Approach: native media in Go, decode and render in the webview

| | Native (Go + client/media) | Webview only (quicvideo/web: WebTransport + WebCodecs) |
|---|---|---|
| macOS (WKWebView) | ScreenCaptureKit, AVFoundation, VideoToolbox: done in phase 3 | WebTransport needs Safari/macOS **26.4+** |
| Windows (WebView2) | WGC + Media Foundation: CI-verified only | works (Chromium) |
| Linux (WebKitGTK 6.0) | portal + PipeWire, V4L2, NVENC/VA-API/openh264 | **no WebTransport** |
| Fits boson's voice | same pattern as `VoiceService` | a second, separate connection stack in JS |

So: **a Go `VideoService` next to `VoiceService`** owns the quicvideo connection, capture and encode. Received video goes to the webview **still encoded**, and the webview decodes it with WebCodecs `VideoDecoder` and draws to a `<canvas>` per tile.
- **Why not decode in Go:** that would mean pushing raw frames into a webview, at roughly 30 fps × tiles × width × height × 4 bytes, through IPC that isn't built for it.
- **Why not webview only:** it can't reach Linux at all, and it would cut off macOS before 26.4.
- **For browsers later:** quicvideo/web remains the path for a web client.

### Pieces

1. **Dependencies.** Bump `github.com/boson-chat/quicvoice` to 1.8 (for `client.VideoTicket`) and add `github.com/boson-chat/quicvideo` (`client`, `client/media`). The `voiceConn` interface gets `VideoTicket(ctx)`.
2. **`desktop/video.go` (VideoService).**
   - **Starting:** after a voice join succeeds, ask for a ticket.
     - On `FORBIDDEN "isn't enabled"`, mark video unavailable for that server and hide the buttons.
     - Otherwise `client.Dial(ticket.quic_url, token)` and `Join(ticket.room)`.
   - **Tickets:** refresh before `expires_at`: get a new ticket from voice, then `Refresh`.
   - **Leaving:** on voice leave or switching channels, close video. On `Error{KICKED}` from quicvideod, tear down and don't redial until the next voice join.
   - **Publishing:**
     - camera: `client/media` camera capture, `cameraLayers` ladder, `Publish(CAMERA)`;
     - screen: `client/media` screen capture, `Publish(SCREEN)`, with the source picked by the OS where possible (see the open questions);
     - stop: unpublish.
   - **Receiving:**
     - `Events()` carries the tracks in the room, mapped to voice participants by user id;
     - `Subscribe(track, maxLayer)` follows the tile sizes the UI reports, using the `layerForTile` rule from web/layers.ts.
   - **Errors:** surface them like voice does (`VoiceState`-style state plus an `onState` event).
3. **Frame transport to the webview.** A loopback WebSocket bound to 127.0.0.1 on a random port, with a per-launch secret in the URL that is handed to the UI through the bridge.
   - Binary messages carry `[track id, flags, capture µs, AU]`, plus "keyframe needed" from the UI back to Go (decoder reset, see `ReportDecodeStall`).
   - Wails events carry JSON; base64 at roughly 10 Mbps across eight tiles is wasteful.
   - Streaming through the asset server's custom scheme isn't reliably incremental on WKWebView.
4. **UI.**
   - **`VideoBridge`** (`window.bosonVideo`): `available`, `setCamera(on, deviceId)`, `setScreen(on, source?)`, `cameras()`, `tiles(sizes)`, `onState`.
   - **`VoiceBar`:** camera and screen-share toggles next to mute/deafen, shown when video is available.
   - **`VoiceSection` / `VoiceRooms`:** camera/screen icons on participants.
   - **Call stage:** a view in the main area that opens when anyone in the channel has video. It uses the grid / pinned-plus-strip layout from web/layers.ts `layout()`. It includes a self-view, which is your own lowest layer fed back locally and not sent through the relay. Tiles report device-pixel sizes back for layer selection.
   - **Decode:** the decode, layout and layer code from quicvideo/web, moved into a small shared package or copied into `modules/video/`.
5. **Packaging and permissions.**
   - **macOS:**
     - `NSCameraUsageDescription` in Info.plist and Info.dev.plist;
     - the `com.apple.security.device.camera` entitlement;
     - Screen Recording is a TCC prompt (no plist key) that ScreenCaptureKit triggers, and on macOS 15+ `SCContentSharingPicker` avoids it for picked content.
     - The app is hardened-runtime signed, so the entitlements must be in the signed build.
   - **Windows:** camera privacy settings; WGC needs Windows 10 1903+.
   - **Linux:**
     - `client/media` links FFmpeg (libavcodec, libavformat, libavdevice, libavutil, libswscale), openh264 and libpipewire-0.3;
     - the nfpm package deps grow accordingly. Linking FFmpeg into the boson binary is a packaging and licensing decision; the alternative is building video as a separate helper;
     - VA-API and NVENC come through FFmpeg;
     - the screen picker is the portal's own dialog.
   - **CI:** `desktop.yml` already builds natively with cgo. It also needs the quicvideo C deps (macOS frameworks are present, Linux needs the FFmpeg, openh264 and libpipewire-0.3 `-dev` packages, Windows uses MinGW as now).
6. **Mobile:** out of scope, like voice (no `bosonVoice` on iOS/Android).

### Order of work (each a boson PR)

1. Deps, `VideoService` with tickets, join, refresh and teardown, plus Go tests with a fake voice conn and an in-process quicvideod (as quicvideo's tests do).
2. Receive path: frame WebSocket, `modules/video` decode and canvas, call stage, `tiles()` → `Subscribe`. A Playwright test with a go-bot publisher, like web/'s.
3. Camera publish plus self-view, macOS plist and entitlements.
4. Screen share (OS pickers), the `VoiceBar` buttons, participant icons.
5. Linux packaging and Windows hardware test.

### Open questions

- **WebCodecs in WebKitGTK 6.0:** not verified; it isn't installed on the CachyOS test box. Without it, Linux needs Go-side decode (`client/media`'s libavcodec decoder) and a raw-frame path (WebGL texture uploads from the WebSocket) as a fallback. Check before step 2.
- **macOS versions:** boson's minimum is macOS 12.0 (`LSMinimumSystemVersion`, `-mmacosx-version-min=12.0`).
  - ScreenCaptureKit needs 12.3, and `SCContentSharingPicker` needs 14.
  - WKWebView's `VideoDecoder` needs WebKit 16.4 (Safari 16.4, which also shipped for Monterey).
  - Either raise the minimum for video, or hide the buttons below those versions. Check `VideoDecoder` inside WKWebView itself, not only in Safari.
- **Screen picker UX on Windows:** the WGC `GraphicsCapturePicker` versus our own window list.
- **Windows minimum:** WGC needs Windows 10 1903+; check against boson's Windows target.
