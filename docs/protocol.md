# quicvideo protocol (v1, ALPN `boson-video/1`)

One QUIC connection per client, TLS 1.3, UDP port 7710 by default.

## Streams

| Stream | Opened by | Content |
|---|---|---|
| first bidirectional | client | control messages (protobuf, `proto/quicvideo/v1/control.proto`), each framed as `varint length` + `ControlMessage`; QUIC urgency 0 |
| unidirectional, client → server | publisher | one **group** of one layer of one of its tracks |
| unidirectional, server → client | relay | one group of a track the client subscribed to |

## Handshake

1. Client opens the control stream and sends `Hello{token}` within `HELLO_TIMEOUT` (5 s).
2. Server verifies the token and answers `Welcome{session_id, limits, token_expires_at}`, or a fatal `Error` (UNAUTHENTICATED / TOKEN_EXPIRED), then closes.

**Token renewal:** send `Refresh{token}` (same user) at any time; the answer is `RefreshOK{token_expires_at}` or a non-fatal `Error` whose message starts with `refresh:`. When the token expires, the server sends a non-fatal `Error{TOKEN_EXPIRED}`. After `TOKEN_GRACE` (30 s) without a successful refresh, it sends a fatal one and closes with code 2.

Tokens are JWTs: EdDSA tokens from the boson auth service (JWKS, audience `quicvideo`), or HS256 test tokens (`quicvideod token`). Claims: `sub`, `name`, `role` (listener = watch only), `rooms` (allowed rooms, empty = any), `perms` (when present, publishing needs `video.publish`).

## Rooms

- `Join{room}` → `RoomState{participants, tracks}` (or `Error`). Rooms are created on the first join and removed when they're empty. Everyone else gets `ParticipantJoined`.
- `Leave` / disconnect → others get `ParticipantLeft`, plus `TrackRemoved` for each of the leaver's tracks.

## Publishing

- `Publish{client_ref, kind (CAMERA|SCREEN), codec, layers[]}` → `PublishOK{client_ref, track_id}` or `Error{client_ref}`. Each layer has an `id` (0 is the lowest by convention), resolution, fps and a target bitrate.
- The rest of the room gets `TrackAdded{track}`.
- `Unpublish{track_id}` → `TrackRemoved` to the room.
- `PublishOK` is always sent before the rest of the room hears `TrackAdded`, so `LayerActive` / `KeyframeRequest` for the new track never arrive first.
- `KeyframeRequest{track_id, layer}` (server → publisher): start a new group on that layer soon. Sent at most once per `KEYFRAME_REQUEST_INTERVAL` (1 s) per layer: when a subscriber has nothing usable to start from (no recent cached group), on an up-switch to a layer with a long GOP, or after a group was dropped.
- `LayerActive{track_id, layer, active}` (server → publisher): whether anyone wants the layer. Layers start inactive. `active: true` comes as soon as someone does; `false` after `LAYER_IDLE_TIMEOUT` (2 s) with nobody. Publishers should stop encoding inactive layers and start an active one with a keyframe.

## Subscribing

- `Subscribe{track_id, layer, priority}`, sent again to change it. **`layer` is the highest layer wanted**; the server forwards the best layer at or below it that its bandwidth estimate for this client allows (see docs/design.md), and may pause a camera that doesn't fit at all. Switches take effect at a group start. A down-switch replays the lower layer's cached group if it started within 1 s; an up-switch waits for the next keyframe.
- `SubscriptionState{track_id, layer, target_layer, max_layer, paused, estimate_bps, forwarding}` (server → subscriber): what the client is getting. Sent whenever layer, target, max, paused or forwarding changes.
- `priority` 0 = server default (screen 1, camera 4); 1..7 = QUIC stream urgency, lower first. The control stream is always 0.
- `Unsubscribe{track_id}`.
- `ReceiverReport` (client → server, about once a second per subscribed, unpaused track): frames, cut groups, mean relative delay, late frames (relative delay > 150 ms), stall time, decode stalls reported by the app, and the interval covered. Relative delay is arrival minus capture minus the recent minimum, so clock offsets cancel. Two bad reports in a row count as congestion when the server also sees queueing, or when the stall is over 1 s.

## Media streams

```
stream:  GroupHeader Frame*
header:  varint track_id, varint layer, varint group_seq
frame:   byte flags, varint capture_ts_us, varint len, payload[len]
flags:   bit 0 = keyframe (other bits must be 0)
```

- A group is a keyframe plus the frames that depend on it. The first frame must be a keyframe.
- `group_seq` increases per layer. The publisher picks it; the relay forwards it unchanged.
- `capture_ts_us` is the sender's wall clock in µs since the Unix epoch. It is meant for latency stats now and for lip sync with quicvoice later.
- Payload is opaque to the server: the codec bitstream (H.264 Annex-B access unit for `h264`) or an end-to-end encrypted blob.
- Max frame 4 MB (`MAX_FRAME_SIZE`).
- A clean FIN ends a group normally. A reset (RESET_STREAM) means the group was cut short: drop the partial group and wait for the next group start.

Stream error codes:

| Code | Meaning |
|---|---|
| 0x10 dropped | a newer group superseded it, or the receiver fell too far behind |
| 0x11 stale | the group is older than the layer's current one |
| 0x12 protocol | the stream broke the media format |
| 0x13 gone | the track or subscription ended |

Connection close codes: 0 ok, 1 protocol, 2 auth, 3 slow consumer (control queue full), 4 shutdown.
