# quicvideo design

quicvideo is the video half of boson calls: friends joining a live call with webcams and screen shares. The expected scale is **small calls of 2–8 people and a handful of calls at once**, not big public rooms or broadcasts. The defaults are set for that: `ROOM_MAX_PARTICIPANTS=20`, a 2 MB send backlog per subscription, and screen share at the top media priority.

It's a standalone service for now. Later it merges with quicvoice so that voice and video are one feature in the UI.

## Shape

- **Relay only (SFU), no transcoding.** Publishers send simulcast (2–3 layers of the same video at different sizes). Each viewer says the best layer it wants per track; the server forwards the best layer at or below that which the viewer's connection can take (see bandwidth control below). The server only forwards. Server CPU therefore scales with packets forwarded; there's no encoding work on the server.
- **One QUIC stream per group of pictures**, per layer (Media-over-QUIC style). Groups can be dropped as a unit without breaking decoding, and QUIC handles loss recovery within a group.
- **Stream priorities** use quic-go's RFC 9218 support (`SetPriority(urgency, incremental=false)`). Urgencies are strict: control 0, screen 1, camera 4 (subscribers can override 1..7). Within one urgency, lower stream IDs go first, which means older groups finish first. On the publisher's uplink, lower layers outrank higher ones (urgency + rank), because every viewer can use them.

## Relay internals (`server/internal/relay`)

- **Track / layer state.** Each layer keeps its current group number and a cache of that group's frames (`GROUP_CACHE_BYTES`, 2 MB). A new subscriber gets the cached group replayed immediately, so it sees a picture without waiting for the next keyframe. A replay only happens from a layer that is live (a frame within 1 s; an inactive layer's cache is cleared). On a layer switch, it only happens if the group started within 1 s, since replaying a long partial GOP shows old frames and costs bandwidth exactly when it's short. Otherwise, or with no usable cache, the relay sends the publisher a `KeyframeRequest`, rate-limited to 1/s per layer.
- **Stale groups.** A group older than the layer's current one is refused (`CancelRead` with code stale).
- **Subscriptions.** Each subscription has a byte-capped queue (`SUB_QUEUE_BYTES`, 2 MB) and one forwarder goroutine that writes groups to the subscriber, one outgoing stream per group.
  - **Layer switch:** happens at the target layer's next group start. Until then, the old layer keeps flowing.
  - **Superseded:** if a new group starts while the previous group still has queued frames, the backlog is dropped and the old stream is reset (`CancelWrite`), so QUIC stops sending and retransmitting stale video. This is the main latency guard.
  - **Overflow:** a group that would push the queue over its cap is dropped (stream reset). The relay ignores the rest of that group and asks for a keyframe.
- **Ingress limit.** A token bucket per track allows `INGRESS_SLACK` (2×) the declared bitrate, with ≥ 4 MB burst so keyframes fit. A group that goes over it is cut.
- **Layer activity.** A layer nobody targets or receives is reported to its publisher as `LayerActive{active: false}` after `LAYER_IDLE_TIMEOUT` (2 s, so a switch in progress doesn't toggle it), and `active: true` as soon as someone wants it. Layers start inactive. The SDK refuses frames for inactive layers (`ErrLayerInactive`) so the app can skip encoding them, which saves upload; small calls often watch only one or two layers of each camera.
- **Locks.** Order is `Relay.mu → layer.mu → Subscription.mu`. `Peer.SendControl` never blocks; a full control queue disconnects the client as a slow consumer.
- **Metrics.** The hot path only does atomic adds; values are exported through OTel observable instruments (Prometheus `/metrics` on :8080, OTLP push optional). Exported: connections, sessions, rooms, tracks (by kind), subscriptions, ingress/egress bytes/frames/groups, drops by reason (`superseded`, `overflow`, `stale`, `rate_limit`, `waiting_keyframe`, `write_error`), keyframe requests and layer switches.

## Bandwidth control (phase 2, `controller.go`)

Each member, in its role as a subscriber, has an estimate of the bits/s its downlink can take. Every 250 ms the relay updates it and hands it out over the member's subscriptions in priority order. Screen shares come first, then by urgency. Every subscription first gets its lowest layer while the budget lasts (a screen share always does); then, in the same order, each one goes as high as the rest of the budget allows, up to its max layer. A camera that doesn't fit even its lowest layer is **paused**. What each viewer actually gets is reported in `SubscriptionState` (layer, target, max, paused, estimate).

**Inputs**
- The delivered rate over the last second: bytes the member's forwarders wrote.
- Send-queue backlog: the age of the oldest queued frame.
- Same-layer group cuts: superseded or overflow. A switch to another layer doesn't count.
- quic-go `ConnectionStats`: smoothed RTT minus the minimum RTT is the network's queueing delay. Loss is only used to hold off probing.
- `ReceiverReport`s, which the SDK sends once a second per track: frames, late frames (relative delay over 150 ms; the minimum delay is subtracted so clock offsets cancel), stall time, decode stalls reported by the app, and cut groups.

**Step down fast.** Congestion is any of:
- a backlog over `BACKLOG_DOWN` (150 ms) for 3 ticks in a row, or over 600 ms at once;
- a same-layer cut;
- RTT inflation over 250 ms;
- two bad receiver reports in a row that the server corroborates (it sees some queueing too, or the viewer stalled over 1 s).

On congestion the estimate drops to 85% of what the current layers need. If the sender was backlogged, the delivered rate is the link's capacity: the estimate goes to 90% of it (no lower than half the need per step, unless the backlog is over 600 ms, which means the link just fell off a cliff, so it goes straight there). That capacity is remembered. Layers step down at once, from the lower layer's cache when it's recent. **A step down never moves another subscription up**, even if a screen share dropping freed budget; the next probe hands that out. A 1 s cooldown follows so a draining backlog doesn't count twice.

**Step up slowly.** After `UP_HOLD` (6 s) without congestion (`STARTUP_HOLD`, 2 s, until the first congestion), the controller probes. It raises the estimate just enough for the cheapest next layer step (×1.5 during startup) and switches up at that layer's next keyframe, asking for one if the GOP is over 1 s. Probes whose smallest step is over 115% of the capacity measured in the last 40 s are skipped. A probe that's followed by congestion within 3 s doubles the hold, up to `MAX_HOLD` (40 s).

**Ingress health.** The relay also watches each layer's arrival delay. It takes arrival minus capture time, minus the track-wide minimum over two 5 s windows (so the publisher's clock offset cancels), smoothed. A layer whose frames arrive more than 250 ms later than the track's best is **unavailable** for 15 s (doubling to 60 s on repeats; a minute of health resets it). Allocations skip it and current viewers step down at once. This catches the case the viewer's own signals can't see: the *publisher's* uplink is the bottleneck.

**Why it doesn't flap:** the estimate only rises by probing and only falls on congestion.

**Things that turned out to matter in the netem runs** (each one was a real failure first):
- A keyframe is a burst. A 1080p screen-share keyframe takes about 1 s to drain at 1 Mbps, so a single 150 ms backlog sample isn't congestion; a sustained one is. One bad second in a receiver report is usually a keyframe too.
- The delivered rate is only the capacity while the sender was backlogged. Otherwise it's just what the layers needed, and feeding it back makes the estimate collapse.
- Receiver reports describe the previous second. After a step down they still describe the old congestion, so they only count once they postdate the step.
- The relative-delay baseline has to forget. After a bandwidth drop, or a camera that was paused, the old minimum makes every frame look late.

## Publisher send side (SDK)

`LocalTrack.WriteFrame` never blocks: each layer has a queue drained by its own goroutine. When the oldest queued frame is older than 300 ms, or the queue would pass 2 MB, the SDK drops frames until the next keyframe. What's already queued still goes out, as a valid prefix of the group. The SDK then fires the keyframe-needed callback (`KeyframeRequests()`, `OnKeyframeNeeded`). A keyframe arriving into a backed-up queue discards the stale frames ahead of it. `Stats()` reports per layer: active, frames and bytes sent, dropped, inactive-refused, groups, keyframes needed, and queue frames/bytes/delay.

## Tokens

`Welcome.token_expires_at` says when the token expires. When it does, the server sends a non-fatal `Error{TOKEN_EXPIRED}` and closes the session after `TOKEN_GRACE` (30 s) unless a `Refresh{token}` arrives. A refresh must be for the same user (`sub`). Its rooms claim is re-checked: if the new token doesn't allow the current room, the member leaves. `Options.TokenSource` makes the SDK refresh a minute before expiry, or at 80% of a shorter lifetime, and right away on the warning.

## Bad networks (`make netem`)

`netem/run.sh` (`make netem`, `MODE=trace make netem`) runs the server, a camera publisher (150k/500k/1.2M, 30 fps, 2 s GOP) and a screen-share publisher (720p 5 fps 300k / 1080p 15 fps 2M, 4 s GOP) in Docker on this Mac. Shaping uses `tc` inside the containers (`NET_ADMIN`).

- **Viewer downlink:** shaped on the server's interface toward the viewer, so the one-way delay scenario makes an RTT of about 100 ms.
- **`uplink1`:** shapes the camera publisher's uplink instead.
- **`capdrop`:** starts at 6 Mbps and drops to 1 Mbps at t = 15 s. Recovery is measured from there; the timing is ±0.2 s from `docker exec` start-up.

`examples/netbench` subscribes to both tracks at their top layer and measures after a 10 s warmup (or after the drop).
- **fps / latency:** latency is arrival minus capture; the containers share a clock.
- **Freeze:** time past a gap of max(250 ms, 3 frame intervals).
- **Recovery:** the first 2 s with ≥ 70% of the layer's fps and p90 latency < 400 ms.

It runs in two modes:

- **`MODE=synthetic`** (default): the bots send synthetic frames at the full layer bitrates continuously. This is the worst case: a camera always moving, a screen always changing.
- **`MODE=trace`**: the bots replay **real VideoToolbox output**. `examples/vtrace` captured a real camera (an iPhone via Continuity, 1080p) and a real working desktop on an M1 Max for 60 s each, and ran every simulcast layer through the same encoder setup as the Publisher. It recorded each frame's size, timing and keyframe flag, plus the size of the same picture encoded as a forced keyframe, so a replayed keyframe request gets a keyframe of the right size. Only sizes and timings are kept (`netem/traces/*.json`), no pictures. VideoToolbox can't run inside the Linux containers, so this replay is the faithful way to put real encoder output on the shaped link.

The recordings show real traffic is much gentler than synthetic:
- **Camera:** about 23 fps, keyframes every 2 s, and the encoder reached only ~60% of the layer targets on real content (691 vs 1160 kbps at 720p).
- **Desktop:** mostly static, about 1 frame/s with real gaps, and occasional big keyframes.

For the screen in trace mode, gaps are content, not freezes, so its freeze and recovery read "n/a".

**Synthetic** (ideal with no limits: screen 1080p at 15 fps, 2.0 Mbps, plus camera 720p at 30 fps, 1.2 Mbps):

| Scenario | Track | Received layer | fps | kbps | p50 ms | p99 ms | Freeze | Recovery |
|---|---|---|---|---|---|---|---|---|
| baseline | camera | L2 720p | 30.0 | 1200 | 1 | 5 | 0.0 s | – |
| baseline | screen | L1 1080p | 15.0 | 2000 | 1 | 6 | 0.0 s | – |
| 2% loss | camera | L2 720p | 30.0 | 1201 | 1 | 28 | 0.0 s | – |
| 2% loss | screen | L1 1080p | 15.0 | 2000 | 1 | 9 | 0.0 s | – |
| 5% loss | camera | L2 720p | 29.9 | 1198 | 1 | 31 | 0.0 s | – |
| 5% loss | screen | L1 1080p | 14.9 | 1993 | 1 | 34 | 0.0 s | – |
| 100 ms ±20 ms delay | camera | L2 720p | 30.0 | 1198 | 113 | 156 | 0.0 s | – |
| 100 ms ±20 ms delay | screen | L1 1080p (L0–L1) | 13.1 | 1709 | 122 | 167 | 0.2 s | – |
| 3 Mbps cap | camera | L1 360p (L1–L2) | 28.8 | 720 | 3 | 532 | 1.3 s | – |
| 3 Mbps cap | screen | L1 1080p (L0–L1) | 12.9 | 1657 | 28 | 302 | 1.3 s | – |
| 1 Mbps cap | camera | L1 360p | 30.0 | 500 | 2 | 462 | 1.9 s | – |
| 1 Mbps cap | screen | L0 720p | 5.0 | 300 | 26 | 415 | 2.0 s | – |
| 6 → 1 Mbps at 15 s | camera | L1 360p (L0–L2) | 25.4 | 416 | 2 | 864 | 4.0 s | 2.4 s |
| 6 → 1 Mbps at 15 s | screen | L0 720p (L0–L1) | 5.0 | 320 | 2 | 796 | 1.2 s | 1.8 s |
| publisher uplink 1 Mbps (camera) | camera | L0 180p (L0–L2) | 18.0 | 246 | 1 | 439 | 0.0 s | – |
| publisher uplink 1 Mbps (camera) | screen | L1 1080p | 15.0 | 2000 | 1 | 5 | 0.0 s | – |

**Real encoder output** (`MODE=trace`):

| Scenario | Track | Received layer | fps | kbps | p50 ms | p99 ms | Freeze | Recovery |
|---|---|---|---|---|---|---|---|---|
| baseline | camera | L2 720p | 24.1 | 975 | 1 | 6 | 0.0 s | – |
| baseline | screen | L1 1080p | 1.2 | 346 | 1 | 2 | n/a | – |
| 2% loss | camera | L2 720p | 22.5 | 386 | 1 | 36 | 0.3 s | – |
| 2% loss | screen | L1 1080p | 1.1 | 341 | 2 | 98 | n/a | – |
| 5% loss | camera | L2 720p | 23.9 | 974 | 1 | 33 | 0.0 s | – |
| 5% loss | screen | L1 1080p | 1.2 | 346 | 2 | 43 | n/a | – |
| 100 ms ±20 ms delay | camera | L2 720p | 22.3 | 376 | 113 | 152 | 0.3 s | – |
| 100 ms ±20 ms delay | screen | L1 1080p | 1.0 | 329 | 142 | 188 | n/a | – |
| 3 Mbps cap | camera | L2 720p | 23.8 | 959 | 1 | 267 | 1.1 s | – |
| 3 Mbps cap | screen | L1 1080p | 1.1 | 334 | 131 | 279 | n/a | – |
| 1 Mbps cap | camera | L2 720p | 22.0 | 384 | 1 | 559 | 1.2 s | – |
| 1 Mbps cap | screen | L0 720p (L0–L1) | 1.0 | 117 | 67 | 1077 | n/a | – |
| 6 → 1 Mbps at 15 s | camera | L1 360p (L1–L2) | 22.3 | 187 | 1 | 808 | 1.3 s | 2.7 s |
| 6 → 1 Mbps at 15 s | screen | L0 720p (L0–L1) | 0.7 | 92 | 53 | 1454 | n/a | – |
| publisher uplink 1 Mbps (camera) | camera | L1 360p (L1–L2) | 20.8 | 319 | 1 | 423 | 0.2 s | – |
| publisher uplink 1 Mbps (camera) | screen | L1 1080p | 1.0 | 293 | 1 | 4 | n/a | – |

(One 30–45 s run per row on this Mac; the kbps column wobbles a lot in trace mode, because each run starts somewhere different in the 60 s recording. Rerun with `MODE=… make netem`.)

Reading it:
- **Loss and delay** cost only latency in both modes: the top layers hold. QUIC repairs loss inside each group, and random loss isn't treated as congestion.
- **Caps:** with real content the camera keeps **720p** even at 3 Mbps, and at 1 Mbps too (it averages ~400 kbps there). Synthetic content needs the full 1.2 Mbps, so it drops to 360p, while the screen holds 1080p by priority.
- **6 → 1 Mbps:** recovers in about 2.5 s in both modes.
  - The first synthetic run of this table **didn't recover**: the camera stayed paused for 28 s. At the drop, loss collapsed the delivered rate for a second (465 kbps). 90% of it was just below what screen-low + camera-low need (450 kbps), so the camera was paused, and probes to resume it kept failing on screen-keyframe bursts.
  - Fix: pausing someone's video is a much bigger hit than a slight overshoot, so the estimate no longer goes below the sum of the lowest layers while the link still delivers that much. The rows above are after the fix; two more runs recovered in 2.4 s and 2.7 s.
- **Uplink limit:** ingress health takes the late top layer away, and the camera settles on a lower one.

## Load test

Run with `examples/loadtest`: every participant publishes a synthetic 3-layer camera (150k / 500k / 1.2M) and subscribes to everyone else in its call at one layer. The initial estimate is set high, so this measures forwarding cost, not the controller. With `LayerActive`, publishers only send the layer people watch.

**Linux, real hardware** (the figure to size with): AMD Ryzen 9 5900XT (CachyOS, bare metal). The server was pinned to 4 cores and the load generator to 12 other cores (`taskset`), over loopback. CPU is the server's steady-state user+system time from `/proc` over 12 s (1.0 = one core).

| Scenario | Forwarded streams | Publish | Egress | Server CPU | Latency p50 / p99 | Groups cut |
|---|---|---|---|---|---|---|
| 4 calls × 6 people, mid layer (500k) | 120 | 12 Mbps | 60 Mbps | 0.17 core | 0.2 / 0.4 ms | 0 |
| 4 calls × 6 people, top layer (1.2M) | 120 | 29 Mbps | 144 Mbps | 0.19 core | 0.2 / 0.5 ms | 0 |
| 8 calls × 8 people, top layer (1.2M) | 448 | 77 Mbps | 537 Mbps | 0.54 core | 0.2 / 0.8 ms | 0 |

That's about **0.1 core per 100 Mbps of egress at load**. At small loads the fixed per-connection work dominates.

Earlier figures, for reference:
- **Linux containers in Docker's VM on the M1 Max** (pinned, `netem/loadtest.sh`): 0.16 core per 100 Mbps at 537 Mbps.
- **macOS loopback** with the generator unpinned (phase 1): 0.5–0.7 core per 100 Mbps.

A real NIC adds interrupt and checksum work that loopback doesn't have, so budget some headroom.

## Hardware for the friends scale

Traffic per call, assuming everyone shares their camera and each viewer gets the others at one layer:

| Call | Egress |
|---|---|
| 4 people, top layer (1.2M) | 4×3×1.2 ≈ 15 Mbps |
| 6 people, grid layer (500k) | 6×5×0.5 = 15 Mbps; ~36 Mbps if everyone watches the top layer |
| 8 people, grid layer | 8×7×0.5 = 28 Mbps |
| + one screen share at 2–3 Mbps | + (viewers × 3) Mbps, e.g. +15–20 Mbps |

A busy evening with **3–5 calls of 4–8 people** comes to about **50–150 Mbps egress**. Ingress is small: each publisher sends roughly 1.9 Mbps for the three layers.

| | Recommendation |
|---|---|
| CPU | **2 dedicated vCPU** is plenty. At the real-hardware figure above, 150 Mbps of egress is about a fifth of a core; the second core is headroom. Dedicated rather than shared, because noisy neighbours cause frame jitter. |
| RAM | **2 GB** is plenty. The relay holds ≤ 2 MB of cache per layer and ≤ 2 MB of queue per subscription, so tens of MB at this scale. |
| Network | A **≥ 300 Mbps** port, and public UDP 7710 open. |
| Traffic | 100 Mbps for 3 h/day is about 4 TB/month; 50 Mbps for 2 h/day is about 1.4 TB/month. Pick a plan with **2–5 TB included**, or unmetered. |
| Kernel | `net.core.rmem_max` / `wmem_max` ≥ 8 MB (quic-go wants about 7 MB). Use `network_mode: host`, or make sure UDP doesn't go through Docker's userland proxy. |

The current boson VPS (2 shared vCPU, 2 GB) is fine for development and one or two small calls. Once people actually use it, video should move to its own box so a screen share can't starve IRC, auth or voice.

## Client media (phase 3, macOS)

`client/media` splits into platform-neutral Go and platform code:

- **Layer presets.** `CameraLayers(w, h, fps)` gives quarter size at half rate, half size, and full size capped at 720p; `ScreenLayers(w, h)` gives ≤1280 wide at 5 fps and ≤1920 wide at 15 fps. Bitrates come from bits per pixel per frame (camera 0.042, screen 0.07), so 720p30 ≈ 1.16 Mbps and a 1920-wide screen ≈ 2.5 Mbps.
- **`LayerForTile`.** Picks the smallest layer that, fitted into the tile's device pixels, is upscaled by at most ~11%; that becomes the subscription's max layer. Hidden tiles unsubscribe.
- **`Publisher`.** Runs one encoder per **active** layer: created on `LayerActive(true)`, closed on false, so unwatched layers cost neither CPU nor upload.
  - Per-layer fps cap with 15% slack.
  - Forced keyframes on activation, on `KeyframeRequest` or a local send-queue drop, and after the track refuses a frame (`ErrNeedKeyframe` / `ErrBackpressure`).
  - It keeps the last captured picture and re-encodes it as a keyframe right away. ScreenCaptureKit sends nothing while the screen is static, so without this a new viewer of a static screen would wait for the next change.
- **Annex-B ↔ AVCC.** quicvideo carries Annex-B access units (AUD, then SPS/PPS on keyframes, then the slices). VideoToolbox speaks AVCC; the conversion is pure Go.

macOS code (cgo, `_darwin` files; `media_other.go` stubs return `ErrUnsupported`):
- **ScreenCaptureKit.** Display or window capture, scaled to ≤1920 wide, NV12, fps cap. Only `SCFrameStatusComplete` frames are delivered; idle frames are skipped, so a static screen costs nothing.
- **AVFoundation.** Camera list with formats; the closest format that supports the requested fps; NV12; late frames discarded.
- **VideoToolbox encoder.**
  - Hardware with `EnableLowLatencyRateControl`, falling back to plain hardware, then software.
  - Real-time, no frame reordering (no B-frames), Constrained High profile.
  - Average bitrate with a 1.5× one-second cap, max keyframe interval (2 s camera, 4 s screen), `MaxFrameDelayCount` 0.
  - Scaling to the layer size via `VTPixelTransferSession` (letterbox).
  - Forced keyframes via `kVTEncodeFrameOptionKey_ForceKeyFrame`.
- **VideoToolbox decoder.** Rebuilds its session when SPS/PPS change (layer switches change the size). Outputs IOSurface-backed BGRA `CVPixelBuffer`s, which a `CALayer` shows directly (`contents = IOSurface`, no copy).

**viewer** (`examples/viewer`) is the desktop test client, built with Gio so the same UI runs on all three platforms. The session logic is toolkit-independent (`session.go`).
- **Controls:** camera on/off, a screen/window picker, stop sharing, leave.
- **Self-preview:** from the camera's captures, converted to RGBA by `media.ToRGBA` via VTPixelTransferSession.
- **Tiles:** a grid or a pinned big tile plus a strip. Decoded pictures are converted to RGBA at the tile's size and drawn as image ops.
- **Decoding:** one decode goroutine per track that only decodes the newest group (groups can overlap at a switch).
- **History:** it replaced the phase 3 AppKit-only `macviewer`.

- **Default camera:** chosen by `media.DefaultCamera`: built-in, then a physical external camera, then Continuity, then virtual. AVFoundation's `transportType` tells virtual cameras (`virt`) from real ones; the system default is often a virtual camera.
- **ObjC linking:** the Objective-C sources are compiled through a cgo preamble (`-x objective-c`) instead of as `.m` files. cmd/go adds its own `-lobjc` for every `.m` package, so linking with Gio made the linker warn about a duplicate library. CI now fails on any linker warning.

Verified on an M1 Max: the VideoToolbox round trip test (hardware encoder, 1280×720 → 640×360 scaling, forced keyframes with SPS/PPS, colours intact, decoder following a 640×360 → 320×180 switch). Manual runs published camera and screen from one client through a local quicvideod to a second: screen pinned at its top layer, camera in the strip at 360p, self-preview in the corner.

## Client media on Linux

The same `client/media` API, in cgo against the system libraries (no bundled codecs).

**Encode** (`AVEncoder`; `BackendAuto` tries them in this order):
1. **NVENC** via libavcodec `h264_nvenc`: `p2` preset, `ull` tune, CBR, zero latency, no B-frames, forced IDR.
2. **VA-API** via `h264_vaapi` (Intel/AMD): frames are uploaded to VA surfaces.
3. **openh264**, called directly: the software fallback. It's BSD-licensed, and this distro's FFmpeg doesn't wrap it.
4. **libx264**: only when explicitly chosen, or with `QUICVIDEO_ALLOW_GPL=1`. It's GPL, and quicvideo shouldn't become GPL by accident.

Encoders scale and convert with libswscale. Output is normalized to quicvideo's access units (AUD first, SPS/PPS on IDR frames).

**Decode:** libavcodec's H.264 decoder (slice threads only, so no extra frame of delay). Pictures are `AVFrame`s, converted to RGBA for the UI with libswscale.

**Camera:** V4L2 through libavdevice, MJPEG preferred (most webcams only reach HD at 30 fps that way). `/dev/video*` nodes are listed with `VIDIOC_QUERYCAP`, skipping metadata nodes. Ranking: built-in, then USB, then v4l2loopback (virtual).

**Screen:**
- **Wayland (and X11 desktops with a portal):** the xdg-desktop-portal ScreenCast flow over D-Bus (godbus): `CreateSession`, then `SelectSources` (monitor or window, embedded cursor, `persist_mode=2`), then `Start`, then `OpenPipeWireRemote`. Then a PipeWire stream asking for mappable buffers (MemPtr/MemFd) in BGRx/BGRA/RGBx/RGBA/xRGB/xBGR.
  - The portal's restore token is saved per user and per source kind (`~/.config/quicvideo/screencast-restore-token-{monitor,window}`, or `$QUICVIDEO_CONFIG_DIR`). It's offered in the next `SelectSources`, so later shares start without a dialog; a fresh token comes back each time.
  - Some portals (Hyprland) show their picker during `SelectSources`, so that step waits up to 2 minutes for the person too.
- **Plain X11 sessions:** x11grab.

**Verified on CachyOS** (Ryzen 9 5900XT, RTX 4070 Ti SUPER with driver 620.70, Hyprland on Wayland, xdg-desktop-portal-hyprland 1.5):
- **Encode/decode:** NVENC and openh264 round-trip tests pass (scaling, forced keyframes with SPS/PPS, colours). VA-API reports encode as unsupported on NVIDIA, as expected (that driver only decodes), so it's untested on real hardware.
- **Screen:** portal + PipeWire capture of the 3840×2160 desktop, encoded by NVENC.
- **Cross-platform call:** a Linux viewer published an NVENC-encoded camera to a Mac viewer, and decoded the Mac's VideoToolbox screen share and camera.
- **Untested:** there's no webcam on that machine, so V4L2 is untested on real hardware (it would need v4l2loopback, a kernel module).

## Phases

1. **Done: relay working end to end.** Wire format, relay, Go SDK, pub/sub bots (synthetic and real H.264), load test, metrics.
2. **Done: quality under bad networks.** Per-subscriber bandwidth estimate and layer choice (max layer from the client), `SubscriptionState`, `ReceiverReport`, `LayerActive` with publisher-side layer pausing, a non-blocking publisher send queue with stats, token expiry + `Refresh`, the Docker netem harness, and a CPU-pinned Linux load test.
3. **Real capture, encode and decode.**
   - **macOS: done.** ScreenCaptureKit, AVFoundation and VideoToolbox.
   - **Linux: done.** Portal + PipeWire, V4L2, NVENC / VA-API / openh264.
   - **Windows:** next.

   The viewer (Gio) is shared.
4. **Browsers.** WebTransport + WebCodecs on the same stream-per-group design.
5. **Merge with voice.** quicvoiced issues short-lived video tickets (Ed25519/JWKS) for the voice room you're in, so the video server never has to call auth or Ergo. One call UI, a `video` profile in boson-chat/stack, and the option to run it on a separate server.

**Dropped: direct P2P for 2–4 person calls.** It was planned as a later phase. Direct QUIC between participants, with hole punching coordinated over the relay, would have saved relay bandwidth for the smallest calls, at the cost of a second media path to build, test and debug (NAT traversal, fallback, per-peer rate control). At the friends-call scale the relay's cost is small (see the load test), so it was taken out of the plan.
