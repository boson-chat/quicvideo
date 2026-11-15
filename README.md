# quicvideo

Video for boson calls: webcams and screen sharing over QUIC, for friends joining a live call (small calls of 2–8 people). `quicvideod` is a forwarding relay (SFU). Publishers send simulcast layers. Each viewer says the best layer it wants per track, and the server forwards the best one at or below that which the viewer's connection can take. The server never transcodes. Media travels on one QUIC stream per group of pictures, so a late group can be dropped whole and screen share outranks cameras.

Standalone for now; it will merge with [quicvoice](https://github.com/boson-chat/quicvoice) into one call feature.

- [docs/protocol.md](docs/protocol.md): the wire protocol
- [docs/design.md](docs/design.md): relay design, load-test numbers, hardware sizing, phases

## Layout

| Path | |
|---|---|
| `server/` | `quicvideod` (cobra: `serve`, `token`), relay, transport, auth, metrics |
| `client/media` | capture, encode, decode: layer presets (`CameraLayers`, `ScreenLayers`), `LayerForTile`, the `Publisher` (one encoder per *watched* layer), and on macOS ScreenCaptureKit, AVFoundation and VideoToolbox (cgo; stubs elsewhere) |
| `examples/viewer` | desktop client (Gio UI): camera/screen controls with a source picker, self-preview, a grid with a pinned big tile |
| `client/` | Go SDK: `Dial`, `Join`, `Publish` → `LocalTrack.WriteFrame` (non-blocking, per-layer queues), `Subscribe` (max layer), events, `OnFrame`, `Refresh`/`TokenSource`, automatic `ReceiverReport`s |
| `wire/`, `proto/`, `gen/` | control framing, media stream format, protobuf |
| `examples/pub` | publishes a synthetic 3-layer camera, or loops real H.264 files |
| `examples/sub` | subscribes, prints fps/kbps/latency/drops, `-dump` writes H.264 |
| `examples/loadtest` | N calls × M participants in one process |
| `examples/netbench` | viewer for the bad-network tests: layer, fps, latency, freeze, recovery |
| `netem/` | Docker harness: `make netem` (tc netem/tbf scenarios), `netem/loadtest.sh` (CPU-pinned load test on Linux) |

## Try it

```sh
make run                          # server on :7710/udp, self-signed cert, any token accepted
make testdata                     # 3 H.264 layers of ffmpeg's test pattern (needs ffmpeg)
make pub-h264 name=alice          # publish real H.264
make sub name=bob layer=2 dump=out.h264
ffplay -f h264 out.h264
make loadtest rooms=4 n=6
make netem                        # bad-network scenarios in Docker → netem/results/results.md
```

## Desktop viewer

```sh
make viewer                      # or: make viewer-app (bin/viewer.app, ad-hoc signed)
bin/viewer -list                 # displays, windows, cameras (with the default marked), permissions
bin/viewer -insecure -token alice -camera -screen     # start sharing right away
bin/viewer -insecure -token bob                       # or use the buttons
```

- **Top bar:** camera on/off, share screen (pick a display or window), stop sharing, leave, and a status line (including permission problems and how to fix them).
- **Self-preview:** your camera in the bottom-right corner.
- **Tiles:** click one to pin it big; click again to unpin. A screen share starts pinned.
- **Subscriptions:** each tile subscribes to the layer that fits its size in device pixels (`media.LayerForTile`). Tiles that aren't shown, and everything while the window is minimized, are unsubscribed.
- **Default camera:** built-in (FaceTime) first, then a physical USB webcam, then an iPhone, and a virtual camera (OBS, Immersed...) only if there's nothing else. Override with `-camera-id`.
- **Flags:** `-pattern` uses a moving test pattern as the camera (no permissions needed). `-snapshot file.png` writes the tiles, and a real window screenshot when allowed.

The UI is [Gio](https://gioui.org): the same viewer runs on macOS and Linux today, and on Windows once its media backend lands.

**macOS requirements:** macOS 13+ (ScreenCaptureKit, low-latency VideoToolbox), and a hardware H.264 encoder (a software fallback is used otherwise).

**Linux requirements:**
- **Libraries:** FFmpeg (`libavcodec`, `libavformat`, `libavdevice`, `libavutil`, `libswscale`), `openh264` and PipeWire (`libpipewire-0.3`). On Debian/Ubuntu, install the `-dev` packages: `libavcodec-dev libavformat-dev libavdevice-dev libavutil-dev libswscale-dev libopenh264-dev libpipewire-0.3-dev`. The Gio UI also needs X11/Wayland/EGL headers (see `.github/workflows/ci.yml`).
- **Encoder:** NVENC on NVIDIA, VA-API on Intel/AMD, openh264 otherwise. libx264 is GPL and only used with `QUICVIDEO_ALLOW_GPL=1`.
- **Screen sharing:** goes through xdg-desktop-portal, which shows the system picker. The choice is remembered with a restore token in `~/.config/quicvideo/`, so later shares start without asking.
- **Cameras:** V4L2. Your user needs access to `/dev/video*`, usually through the `video` group.

**Permissions (TCC):**
- **Screen Recording:** System Settings → Privacy & Security → Screen & System Audio Recording. The app asks once; after allowing, it must be **restarted** (macOS applies it at launch). There is no Info.plist key for it.
- **Camera:** System Settings → Privacy & Security → Camera. The app needs **`NSCameraUsageDescription`** in its Info.plist; without it macOS kills the process when the camera opens. `examples/viewer/Info.plist` is embedded into the binary (`-sectcreate __TEXT __info_plist`) and used for the `.app`.
- **Started from a terminal:** both permissions belong to the **terminal app**, not the binary.
- **Errors:** missing permissions come back as `*media.PermissionError` (`errors.Is(err, media.ErrScreenPermission / ErrCameraPermission)`) with these instructions.

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
| `INITIAL_BITRATE` | `2500000` | a new subscriber's bandwidth estimate, bits/s |
| `MIN_BITRATE` | `150000` | estimate floor |
| `BACKLOG_DOWN` | `150ms` | send backlog that counts as congestion (sustained over 3 ticks, or 4× at once) |
| `QUEUE_DELAY_DOWN` | `250ms` | RTT above the minimum that counts as congestion |
| `UP_HOLD` / `STARTUP_HOLD` / `MAX_HOLD` | `6s` / `2s` / `40s` | stable time before probing up (before the first congestion / after failed probes) |
| `LAYER_IDLE_TIMEOUT` | `2s` | a layer nobody watches for this long is reported inactive to its publisher |
| `TOKEN_GRACE` | `30s` | after the token expires, time to send `Refresh` before the session is closed |
| `TRACE_CONTROLLER` | `false` | log every bandwidth decision |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | optional OTLP metrics push |

Publishing needs `video.publish` when the token has a `perms` claim; otherwise any role but `listener` can publish. A `rooms` claim limits which rooms can be joined.

Publishers get `LayerActive` messages and should only encode the layers someone watches: the SDK refuses frames for inactive layers with `ErrLayerInactive`. Start a newly active layer with a keyframe. `KeyframeRequests()` (or `PublishOptions.OnKeyframeNeeded`) fires for server requests, newly active layers and local send-queue drops.

## Release

Every push to `main` runs semantic-release (`feat` → minor, `fix`/`perf`/`refactor`/`build` → patch). It builds `harbor.floret.dev/boson/quicvideo:<version>` for amd64 and arm64. This needs `HARBOR_USERNAME` / `HARBOR_PASSWORD`: the org secrets only reach this repo once it's public (free plan); otherwise add them per repo.
