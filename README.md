# quicvideo

Video for boson calls: webcams and screen sharing over QUIC, for friends joining a live call (small calls of 2–8 people). `quicvideod` is a forwarding relay (SFU). Publishers send simulcast layers, each viewer picks one per track, and the server never transcodes. Media travels on one QUIC stream per group of pictures, so a late group can be dropped whole and screen share outranks cameras.

Standalone for now; it will merge with [quicvoice](https://github.com/boson-chat/quicvoice) into one call feature.

- [docs/protocol.md](docs/protocol.md): the wire protocol
- [docs/design.md](docs/design.md): relay design, load-test numbers, hardware sizing, phases

## Layout

| Path | |
|---|---|
| `server/` | `quicvideod` (cobra: `serve`, `token`), relay, transport, auth, metrics |
| `client/` | Go SDK: `Dial`, `Join`, `Publish` → `LocalTrack.WriteFrame`, `Subscribe`, events, `OnFrame` |
| `wire/`, `proto/`, `gen/` | control framing, media stream format, protobuf |
| `examples/pub` | publishes a synthetic 3-layer camera, or loops real H.264 files |
| `examples/sub` | subscribes, prints fps/kbps/latency/drops, `-dump` writes H.264 |
| `examples/loadtest` | N calls × M participants in one process |

## Try it

```sh
make run                          # server on :7710/udp, self-signed cert, any token accepted
make testdata                     # 3 H.264 layers of ffmpeg's test pattern (needs ffmpeg)
make pub-h264 name=alice          # publish real H.264
make sub name=bob layer=2 dump=out.h264
ffplay -f h264 out.h264
make loadtest rooms=4 n=6
```

## Configuration (env)

| Var | Default | |
|---|---|---|
| `QUIC_ADDR` | `:7710` | UDP listen address |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | | cert files (reloaded on change); empty = self-signed dev cert |
| `HTTP_ADDR` | `:8080` | internal: `/healthz`, `/version`, `/metrics` |
| `PUBLIC_HTTP_ADDR` | `:8090` | public: `/healthz` |
| `AUTH_JWKS_URL`, `AUTH_ISSUER`, `AUTH_AUDIENCE` | `quicvideo` audience | boson auth service tokens |
| `JWT_SECRET` | | HS256 test tokens (`quicvideod token --user alice`) |
| `AUTH_INSECURE` | `false` | dev only: any token = user id (`name@listener` = watch only) |
| `ROOM_MAX_PARTICIPANTS` | `20` | |
| `MAX_TRACKS_PER_SESSION` / `MAX_LAYERS` | `4` / `4` | |
| `MAX_TRACK_BITRATE` | `20000000` | sum of a track's declared layers, bits/s |
| `MAX_FRAME_SIZE` | `4194304` | |
| `INGRESS_SLACK` | `2.0` | allowed multiple of the declared bitrate |
| `GROUP_CACHE_BYTES` | `2097152` | cached current group per layer (fast join) |
| `SUB_QUEUE_BYTES` | `2097152` | send backlog per subscription before a group is dropped |
| `KEYFRAME_REQUEST_INTERVAL` | `1s` | per layer |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | optional OTLP metrics push |

Publishing needs `video.publish` when the token has a `perms` claim; otherwise any role but `listener` can publish. A `rooms` claim limits which rooms can be joined.

## Release

Every push to `main` runs semantic-release (`feat` → minor, `fix`/`perf`/`refactor`/`build` → patch). It builds `harbor.floret.dev/boson/quicvideo:<version>` for amd64 and arm64. This needs `HARBOR_USERNAME` / `HARBOR_PASSWORD`: the org secrets only reach this repo once it's public (free plan); otherwise add them per repo.
