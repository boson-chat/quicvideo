# quicvideo design

quicvideo is the video half of boson calls: friends joining a live call with webcams and screen shares. The expected scale is **small calls of 2–8 people and a handful of calls at once**, not big public rooms or broadcasts. The defaults are set for that: `ROOM_MAX_PARTICIPANTS=20`, a 2 MB send backlog per subscription, and screen share at the top media priority.

It's a standalone service for now. Later it merges with quicvoice so that voice and video are one feature in the UI.

## Shape

- **Relay only (SFU), no transcoding.** Publishers send simulcast (2–3 layers of the same video at different sizes). Each viewer picks a layer per track, and the server only forwards. Server CPU therefore scales with packets forwarded; there's no encoding work on the server.
- **One QUIC stream per group of pictures**, per layer (Media-over-QUIC style). Groups can be dropped as a unit without breaking decoding, and QUIC handles loss recovery within a group.
- **Stream priorities** use quic-go's RFC 9218 support (`SetPriority(urgency, incremental=false)`). Urgencies are strict: control 0, screen 1, camera 4 (subscribers can override 1..7). Within one urgency, lower stream IDs go first, which means older groups finish first.

## Relay internals (`server/internal/relay`)

- **Track / layer state.** Each layer keeps its current group number and a cache of that group's frames (`GROUP_CACHE_BYTES`, 2 MB). A new subscriber gets the cached group replayed immediately, so it sees a picture without waiting for the next keyframe. If there's no usable cache (nothing sent yet, the group was too big, or it was aborted), the relay sends the publisher a `KeyframeRequest`, rate-limited to 1/s per layer.
- **Stale groups.** A group older than the layer's current one is refused (`CancelRead` with code stale).
- **Subscriptions.** Each subscription has a byte-capped queue (`SUB_QUEUE_BYTES`, 2 MB) and one forwarder goroutine that writes groups to the subscriber, one outgoing stream per group.
  - **Layer switch:** happens at the target layer's next group start. Until then, the old layer keeps flowing.
  - **Superseded:** if a new group starts while the previous group still has queued frames, the backlog is dropped and the old stream is reset (`CancelWrite`), so QUIC stops sending and retransmitting stale video. This is the main latency guard.
  - **Overflow:** a group that would push the queue over its cap is dropped (stream reset). The relay ignores the rest of that group and asks for a keyframe.
- **Ingress limit.** A token bucket per track allows `INGRESS_SLACK` (2×) the declared bitrate, with ≥ 4 MB burst so keyframes fit. A group that goes over it is cut.
- **Locks.** Order is `Relay.mu → layer.mu → Subscription.mu`. `Peer.SendControl` never blocks; a full control queue disconnects the client as a slow consumer.
- **Metrics.** The hot path only does atomic adds; values are exported through OTel observable instruments (Prometheus `/metrics` on :8080, OTLP push optional). Exported: connections, sessions, rooms, tracks (by kind), subscriptions, ingress/egress bytes/frames/groups, drops by reason (`superseded`, `overflow`, `stale`, `rate_limit`, `waiting_keyframe`, `write_error`), keyframe requests and layer switches.

## Load test (phase 1)

Run with `examples/loadtest`: every participant publishes a synthetic 3-layer camera (150k / 500k / 1.2M) and subscribes to everyone else in its call at one layer. Server CPU was sampled with `ps` (100% = one core).

**Caveat:** this ran on an Apple M1 Max over **loopback**, with the load generator on the same machine competing for CPU. That isn't representative of a Linux VPS with real NICs, so treat the numbers as an order of magnitude only.

| Scenario | Forwarded streams | Ingress | Egress | Server CPU | Latency p50 / p99 | Groups cut |
|---|---|---|---|---|---|---|
| 4 calls × 6 people, mid layer (500k) | 120 | 44 Mbps | 60 Mbps | ~0.7 core | 0.4 / 1.7 ms | 0 |
| 4 calls × 6 people, top layer (1.2M) | 120 | 44 Mbps | 144 Mbps | ~0.85 core | 0.5 / 2.9 ms | 0 |
| 8 calls × 8 people, top layer (1.2M) | 448 | 118 Mbps | 537 Mbps | ~2.5 cores | 5.4 / 120 ms | 34 of 4478 |

That's roughly **0.5–0.7 core per 100 Mbps of egress**, plus fixed costs. In the heaviest run, the machine running both the server and ~64 clients was saturated, which is where the latency tail and the dropped groups came from.

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
| CPU | **2 dedicated vCPU** (4 for headroom). Dedicated rather than shared, because noisy neighbours cause frame jitter. |
| RAM | **2 GB** is plenty. The relay holds ≤ 2 MB of cache per layer and ≤ 2 MB of queue per subscription, so tens of MB at this scale. |
| Network | A **≥ 300 Mbps** port, and public UDP 7710 open. |
| Traffic | 100 Mbps for 3 h/day is about 4 TB/month; 50 Mbps for 2 h/day is about 1.4 TB/month. Pick a plan with **2–5 TB included**, or unmetered. |
| Kernel | `net.core.rmem_max` / `wmem_max` ≥ 8 MB (quic-go wants about 7 MB). Use `network_mode: host`, or make sure UDP doesn't go through Docker's userland proxy. |

The current boson VPS (2 shared vCPU, 2 GB) is fine for development and one or two small calls. Once people actually use it, video should move to its own box so a screen share can't starve IRC, auth or voice.

## Phases

1. **Done: relay working end to end.** Wire format, relay, Go SDK, pub/sub bots (synthetic and real H.264), load test, metrics.
2. **Quality under bad networks.**
   - Server-side bandwidth estimation per subscriber (write backpressure + `ReceiverReport`), with automatic layer down/up switching.
   - Publisher-side layer pausing when nobody watches a layer.
   - `tc netem` loss and delay tests.
   - Token expiry handling.
3. **Real capture and encoding in the client.** macOS first (ScreenCaptureKit + VideoToolbox), then Windows (WGC + Media Foundation/NVENC) and Linux (PipeWire + VAAPI/NVENC), plus a viewer in the testapp.
4. **Browsers.** WebTransport + WebCodecs on the same stream-per-group design.
5. **Direct P2P for 2–4 person calls, falling back to the relay.** For a small friends call, a direct QUIC connection between participants (with NAT hole punching coordinated over the relay's control stream) saves the server's bandwidth and a network hop. Bigger calls, failed hole punching or strict NATs use the relay. The media format stays the same, so the client only switches where it sends.
6. **Merge with voice.** quicvoiced issues short-lived video tickets (Ed25519/JWKS) for the voice room you're in, so the video server never has to call auth or Ergo. One call UI, a `video` profile in boson-chat/stack, and the option to run it on a separate server.
