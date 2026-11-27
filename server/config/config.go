// Package config holds quicvideod configuration, loaded from env vars.
package config

import (
	"time"

	"github.com/jinzhu/configor"
)

type Config struct {
	AppConfig       AppConfig
	QUICConfig      QUICConfig
	HTTPConfig      HTTPConfig
	AuthConfig      AuthConfig
	RoomConfig      RoomConfig
	RelayConfig     RelayConfig
	TelemetryConfig TelemetryConfig
}

// TelemetryConfig controls metrics export. Prometheus on HTTP /metrics is
// always on; OTLP push is on when an OTLP endpoint is set. The OTLP exporter
// also reads the other standard OTEL_EXPORTER_OTLP_* vars itself.
type TelemetryConfig struct {
	OTLPEndpoint        string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTLPMetricsEndpoint string `env:"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"`
	// http/protobuf (default) or grpc.
	OTLPProtocol        string `env:"OTEL_EXPORTER_OTLP_PROTOCOL"`
	OTLPMetricsProtocol string `env:"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"`
	// "none" turns OTLP push off even when an endpoint is set.
	MetricsExporter string `env:"OTEL_METRICS_EXPORTER"`
}

type AppConfig struct {
	APPName string `default:"quicvideod"`
	Env     string `default:"development" env:"ENV"`
	Version string `default:"dev" env:"VERSION"`
}

type QUICConfig struct {
	// UDP listen address for video traffic.
	Addr string `default:":7710" env:"QUIC_ADDR"`
	// TLS cert/key files (reloaded on change). Empty = self-signed (dev only).
	CertFile       string        `env:"TLS_CERT_FILE"`
	KeyFile        string        `env:"TLS_KEY_FILE"`
	DevHostname    string        `default:"localhost" env:"DEV_HOSTNAME"`
	MaxIdleTimeout time.Duration `default:"30s" env:"QUIC_MAX_IDLE_TIMEOUT"`
	KeepAlive      time.Duration `default:"10s" env:"QUIC_KEEPALIVE"`
	// How long a new connection has to send Hello.
	HelloTimeout time.Duration `default:"5s" env:"HELLO_TIMEOUT"`
	// Flow-control windows. Video groups are large (a keyframe can be
	// hundreds of KB), so these are well above quic-go's defaults.
	MaxStreamReceiveWindow     uint64 `default:"8388608" env:"QUIC_MAX_STREAM_WINDOW"`
	MaxConnectionReceiveWindow uint64 `default:"33554432" env:"QUIC_MAX_CONN_WINDOW"`
	// Origins allowed to open WebTransport sessions (comma-separated, "*" =
	// any). Sessions also need a token. "directory" = the origins at
	// ORIGINS_URL (mixable with fixed origins, which stay allowed when that
	// list can't be fetched).
	WebTransportOrigins string `default:"*" env:"WEBTRANSPORT_ORIGINS"`
	// The web origins list for "directory": the auth service's
	// GET /v1/web-origins ({"origins": [...]}), e.g.
	// http://auth:8080/v1/web-origins. Fetched at start and every
	// OriginsRefresh; the last good list stays when it fails.
	OriginsURL     string        `env:"ORIGINS_URL"`
	OriginsRefresh time.Duration `default:"5m" env:"ORIGINS_REFRESH"`
	// Concurrent incoming uni streams (media groups) per connection.
	MaxIncomingUniStreams int64 `default:"256" env:"QUIC_MAX_UNI_STREAMS"`
}

type HTTPConfig struct {
	// Control listener for quicvoiced (voice → video membership: leaves,
	// closed channels, snapshots). Empty = off. Bearer CONTROL_TOKEN.
	ControlAddr  string `env:"CONTROL_HTTP_ADDR"`
	ControlToken string `env:"CONTROL_TOKEN"`
	// Rooms missing from voice's snapshot are emptied too (all video rooms
	// are voice channels).
	VoiceSyncStrict bool `default:"false" env:"VOICE_SYNC"`
	// Health + version + metrics. Internal only.
	Addr string `default:":8080" env:"HTTP_ADDR"`
	// Public listener (GET /healthz), meant to sit behind a TLS reverse
	// proxy. Never serves /metrics. Empty = off.
	PublicAddr string `default:":8090" env:"PUBLIC_HTTP_ADDR"`
}

type AuthConfig struct {
	// HS256 secret for test tokens (`quicvideod token`).
	JWTSecret string `env:"JWT_SECRET"`
	// Accept any token as the user id. Dev/testing only.
	Insecure bool `default:"false" env:"AUTH_INSECURE"`
	// boson auth service JWKS, e.g. https://auth.example.com/.well-known/jwks.json.
	JWKSURL  string `env:"AUTH_JWKS_URL"`
	Issuer   string `env:"AUTH_ISSUER"`
	Audience string `default:"quicvideo" env:"AUTH_AUDIENCE"`
	// After the token expires the client is warned and has this long to
	// send Refresh before the session is closed.
	TokenGrace time.Duration `default:"30s" env:"TOKEN_GRACE"`
}

type RoomConfig struct {
	MaxParticipants int `default:"20" env:"ROOM_MAX_PARTICIPANTS"`
}

// RelayConfig limits what publishers may send and how much the relay
// buffers per subscriber.
type RelayConfig struct {
	MaxTracksPerSession int    `default:"4" env:"MAX_TRACKS_PER_SESSION"`
	MaxLayers           int    `default:"4" env:"MAX_LAYERS"`
	MaxFrameSize        int    `default:"4194304" env:"MAX_FRAME_SIZE"`
	MaxTrackBitrate     uint64 `default:"20000000" env:"MAX_TRACK_BITRATE"`
	// Ingress is cut off (frames dropped) above this multiple of the
	// track's declared bitrate, measured over a 2 s window with burst room
	// for keyframes.
	IngressSlack float64 `default:"2.0" env:"INGRESS_SLACK"`
	// Current group kept per layer for fast join. A group bigger than
	// this stops being cached (joiners then wait for the next keyframe).
	GroupCacheBytes int `default:"2097152" env:"GROUP_CACHE_BYTES"`
	// Per-subscription send backlog before the current group is dropped.
	SubQueueBytes int `default:"2097152" env:"SUB_QUEUE_BYTES"`
	// Minimum interval between keyframe requests for one layer.
	KeyframeRequestInterval time.Duration `default:"1s" env:"KEYFRAME_REQUEST_INTERVAL"`

	// Bandwidth control per subscriber (docs/design.md).
	InitialBitrate   float64       `default:"2500000" env:"INITIAL_BITRATE"`
	MinBitrate       float64       `default:"150000" env:"MIN_BITRATE"`
	BacklogDown      time.Duration `default:"150ms" env:"BACKLOG_DOWN"`
	QueueDelayDown   time.Duration `default:"250ms" env:"QUEUE_DELAY_DOWN"`
	UpHold           time.Duration `default:"6s" env:"UP_HOLD"`
	StartupHold      time.Duration `default:"2s" env:"STARTUP_HOLD"`
	MaxHold          time.Duration `default:"40s" env:"MAX_HOLD"`
	LayerIdleTimeout time.Duration `default:"2s" env:"LAYER_IDLE_TIMEOUT"`
	// Log every controller decision (step down / probe) at info level.
	TraceController bool `default:"false" env:"TRACE_CONTROLLER"`
}

func LoadConfig() (Config, error) {
	var cfg Config
	err := configor.New(&configor.Config{ENVPrefix: "-", Silent: true}).Load(&cfg)
	return cfg, err
}

func LoadConfigOrPanic() Config {
	cfg, err := LoadConfig()
	if err != nil {
		panic(err)
	}
	return cfg
}
