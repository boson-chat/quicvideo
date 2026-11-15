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

`netem/run.sh` runs the server, a camera publisher (150k/500k/1.2M, 30 fps, 2 s GOP) and a screen-share publisher (720p 5 fps 300k / 1080p 15 fps 2M, 4 s GOP) in Docker on this Mac. Shaping uses `tc` inside the containers (`NET_ADMIN`).

- **Viewer downlink:** shaped on the server's interface toward the viewer, so the one-way delay scenario makes an RTT of about 100 ms.
- **`uplink1`:** shapes the camera publisher's uplink instead.
- **`capdrop`:** starts at 6 Mbps and drops to 1 Mbps at t = 15 s. Recovery is measured from there; the timing is ±0.2 s from `docker exec` start-up.

`examples/netbench` subscribes to both tracks at their top layer and measures after a 10 s warmup (or after the drop).
- **fps / latency:** latency is arrival minus capture; the containers share a clock.
- **Freeze:** time past a gap of max(250 ms, 3 frame intervals).
- **Recovery:** the first 2 s with ≥ 70% of the layer's fps and p90 latency < 400 ms.

Ideal with no limits: screen 1080p at 15 fps (2.0 Mbps) plus camera 720p at 30 fps (1.2 Mbps), about 3.2 Mbps. Results after tuning:

| Scenario | Track | Received layer | fps | kbps | p50 ms | p99 ms | Freeze | Recovery |
|---|---|---|---|---|---|---|---|---|
| baseline (no limits) | screen | L1 1080p | 15.0 | 1999 | 1 | 6 | 0.0 s | – |
| baseline (no limits) | camera | L2 720p | 30.0 | 1199 | 1 | 4 | 0.0 s | – |
| 2% loss | screen | L1 1080p | 15.0 | 1999 | 1 | 25 | 0.0 s | – |
| 2% loss | camera | L2 720p | 30.0 | 1199 | 1 | 14 | 0.0 s | – |
| 5% loss | screen | L1 1080p | 15.0 | 2000 | 1 | 32 | 0.0 s | – |
| 5% loss | camera | L2 720p | 30.0 | 1200 | 1 | 31 | 0.0 s | – |
| 100 ms ±20 ms delay | screen | L1 1080p | 15.0 | 2000 | 122 | 170 | 0.0 s | – |
| 100 ms ±20 ms delay | camera | L2 720p | 30.0 | 1198 | 114 | 154 | 0.0 s | – |
| 3 Mbps cap | screen | L1 1080p (L0–L1) | 12.9 | 1657 | 23 | 332 | 1.4 s | – |
| 3 Mbps cap | camera | L1 360p (L1–L2) | 28.6 | 690 | 2 | 651 | 1.7 s | – |
| 1 Mbps cap | screen | L0 720p | 4.8 | 292 | 1 | 754 | 0.7 s | – |
| 1 Mbps cap | camera | L0 180p (L0–L2) | 19.4 | 298 | 1 | 1254 | 2.8 s | – |
| 6 → 1 Mbps at 15 s | screen | L0 720p (L0–L1) | 4.8 | 303 | 2 | 535 | 1.5 s | 1.9 s |
| 6 → 1 Mbps at 15 s | camera | L1 360p (L0–L2) | 25.1 | 408 | 2 | 968 | 4.4 s | 2.6 s |
| publisher uplink 1 Mbps (camera) | screen | L1 1080p | 14.5 | 1945 | 1 | 8 | 0.6 s | – |
| publisher uplink 1 Mbps (camera) | camera | L0 180p (L0–L2) | 15.0 | 187 | 1 | 382 | 1.2 s | – |

(One 30 s run each; run-to-run variation is real. Earlier runs of `cap1` settled the camera on 360p rather than 180p, for example. Rerun `make netem` to compare.)

Reading it:
- **Loss (2%, 5%) and delay (100 ± 20 ms)** cost only latency: both tracks stay at their top layers. QUIC repairs the loss inside each group, and the controller doesn't treat random loss as congestion.
- **3 Mbps:** keeps the screen at 1080p and puts the camera on 360p. That's the priority order: screen share first.
- **1 Mbps:** settles on screen 720p at 5 fps plus a small camera. Most freezes and the p99 tail come from the one or two probes allowed after the capacity memory expires.
- **6 → 1 Mbps:** both tracks are back to steady video about 2–2.6 s after the drop. The camera's freeze includes the drop itself.
- **Uplink limit:** the camera publisher can't push its 720p layer through 1 Mbps. The relay notices that layer arriving late (ingress health, see bandwidth control above), stops offering it, and with nobody watching it the publisher stops sending it. The viewer gets a clean low layer instead of a frozen 720p. Before ingress health this scenario gave p50 484 ms and 9 s of freezes on 720p.

## Load test

Run with `netem/loadtest.sh` (`examples/loadtest`): every participant publishes a synthetic 3-layer camera (150k / 500k / 1.2M) and subscribes to everyone else in its call at one layer. The initial estimate is set high, so this measures forwarding cost, not the controller. Server CPU is from `docker stats` (100% = one core).

**Linux** (phase 2; the target figure). The server and the load generator ran in separate Linux containers **pinned to separate CPUs** (server on 4 cores, generator on 6), in Docker Desktop's Linux VM on the M1 Max. It's virtualized and on one machine, so it's still not a VPS NIC, but the Linux network stack and quic-go's Linux paths (GSO, recvmmsg) are real, and the generator no longer competes for the server's cores. The CachyOS box was unreachable for this run.

| Scenario | Forwarded streams | Publish | Egress | Server CPU | Latency p50 / p99 | Groups cut |
|---|---|---|---|---|---|---|
| 4 calls × 6 people, mid layer (500k) | 120 | 12 Mbps | 60 Mbps | ~0.32 core | 0.3 / 1.5 ms | 0 |
| 4 calls × 6 people, top layer (1.2M) | 120 | 29 Mbps | 144 Mbps | ~0.38 core | 0.3 / 1.3 ms | 0 |
| 8 calls × 8 people, top layer (1.2M) | 448 | 77 Mbps | 537 Mbps | ~0.85 core (1.25 while ramping up) | 0.4 / 3.6 ms | 0 |

That's about **0.16 core per 100 Mbps of egress at 500 Mbps**, and 0.25–0.5 at small loads, where fixed per-connection work dominates.

- **Publishing is lower than in phase 1:** with `LayerActive`, publishers only send the layer people watch (phase 1 sent all three: 44 vs 12 Mbps in the first row).
- **Phase 1 on macOS loopback**, with the generator unpinned on the same CPUs, measured 0.5–0.7 core per 100 Mbps. Most of that difference was the generator competing for the cores, plus macOS's UDP path.

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
| CPU | **2 dedicated vCPU** is plenty. At the Linux figure above, 150 Mbps of egress is under half a core; the second core is headroom. Dedicated rather than shared, because noisy neighbours cause frame jitter. |
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

**macviewer** is the test client: AppKit window, one layer-backed tile per remote track, a pinned big tile plus a strip, and a per-track decode goroutine that only decodes the newest group (groups can overlap at a switch). The steps after it are a proper app UI, Windows and Linux, browsers and the voice merge.

Verified on an M1 Max: the VideoToolbox round trip test (hardware encoder, 1280×720 → 640×360 scaling, forced keyframes with SPS/PPS, colours intact, decoder following a 640×360 → 320×180 switch). A manual run published camera and screen from one macviewer through a local quicvideod to a second macviewer: screen pinned, settling on the 1246p layer after the startup probe; camera in the strip at 360p.

## Phases

1. **Done: relay working end to end.** Wire format, relay, Go SDK, pub/sub bots (synthetic and real H.264), load test, metrics.
2. **Done: quality under bad networks.** Per-subscriber bandwidth estimate and layer choice (max layer from the client), `SubscriptionState`, `ReceiverReport`, `LayerActive` with publisher-side layer pausing, a non-blocking publisher send queue with stats, token expiry + `Refresh`, the Docker netem harness, and a CPU-pinned Linux load test.
3. **Done (macOS): real capture, encode and decode.** `client/media` + `examples/macviewer`, see below. Windows (Windows Graphics Capture + Media Foundation/NVENC/AMF/QSV) and Linux (PipeWire portal + VAAPI/NVENC) capture and codecs are next; the `Publisher`, layer presets and tile mapping are platform-neutral already.
4. **Browsers.** WebTransport + WebCodecs on the same stream-per-group design.
5. **Direct P2P for 2–4 person calls, falling back to the relay.** For a small friends call, a direct QUIC connection between participants (with NAT hole punching coordinated over the relay's control stream) saves the server's bandwidth and a network hop. Bigger calls, failed hole punching or strict NATs use the relay. The media format stays the same, so the client only switches where it sends.
6. **Merge with voice.** quicvoiced issues short-lived video tickets (Ed25519/JWKS) for the voice room you're in, so the video server never has to call auth or Ergo. One call UI, a `video` profile in boson-chat/stack, and the option to run it on a separate server.
