package commands

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/boson-chat/quicvideo/server/config"
	"github.com/boson-chat/quicvideo/server/http"
	"github.com/boson-chat/quicvideo/server/internal/certs"
	"github.com/boson-chat/quicvideo/server/internal/logger"
	"github.com/boson-chat/quicvideo/server/internal/origins"
	"github.com/boson-chat/quicvideo/server/internal/relay"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/boson-chat/quicvideo/server/internal/transport"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the QUIC video relay, the public HTTP listener and the health/metrics listener",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.LoadConfigOrPanic()

		l := logger.Logger(
			logger.WithServerName(cfg.AppConfig.APPName),
			logger.WithVersion(cfg.AppConfig.Version),
			logger.WithEnvironment(cfg.AppConfig.Env),
		)

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		ctx = logger.WithLogger(ctx, l)

		provider, err := metrics.NewProvider(ctx, cfg)
		if err != nil {
			return err
		}
		defer shutdownMetrics(ctx, provider)
		if p := provider.OTLPProtocol(); p != "" {
			l.Info().Str("protocol", p).Msg("OTLP metrics push enabled")
		}
		m, err := metrics.New(provider.MeterProvider())
		if err != nil {
			return err
		}

		authService, err := BuildAuth(ctx, cfg.AuthConfig)
		if err != nil {
			return err
		}
		if cfg.AuthConfig.Insecure {
			l.Warn().Msg("AUTH_INSECURE is on: any token is accepted")
		}

		if q := cfg.QUICConfig; q.CertFile != "" {
			err := certs.WaitForFiles(ctx, q.CertFile, q.KeyFile, 2*time.Second, func(reason error) {
				l.Warn().Err(reason).Str("cert", q.CertFile).Msg("waiting for the TLS certificate files (certsync exports them once the certificate exists)")
			})
			if err != nil {
				return err
			}
		}
		tlsConf, err := BuildTLS(cfg.QUICConfig)
		if err != nil {
			return err
		}
		ropts := RelayOptions(cfg, m)
		if cfg.RelayConfig.TraceController {
			ropts.Trace = func(session uint32, msg string) {
				l.Info().Uint32("session", session).Msg("controller " + msg)
			}
		}
		r := relay.New(ctx, ropts)
		quicServer := transport.NewServer(cfg, tlsConf, authService, r, m)
		webOrigins, wtOrigins, err := buildOrigins(ctx, cfg.QUICConfig)
		if err != nil {
			return err
		}
		quicServer.SetOrigins(wtOrigins)

		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error { return quicServer.Serve(gctx) })
		if webOrigins != nil {
			g.Go(func() error { webOrigins.Run(gctx); return nil })
		}
		g.Go(func() error {
			return http.StartServerWithContext(gctx, cfg.HTTPConfig.Addr, cfg.AppConfig.Version, provider.Handler())
		})
		if addr := cfg.HTTPConfig.ControlAddr; addr != "" {
			if cfg.HTTPConfig.ControlToken == "" {
				return errors.New("CONTROL_HTTP_ADDR needs CONTROL_TOKEN")
			}
			g.Go(func() error {
				return http.StartControlServerWithContext(gctx, addr, r, http.ControlOptions{
					Token: cfg.HTTPConfig.ControlToken, Strict: cfg.HTTPConfig.VoiceSyncStrict})
			})
		}
		if addr := cfg.HTTPConfig.PublicAddr; addr != "" {
			g.Go(func() error {
				return http.StartPublicServerWithContext(gctx, addr, http.PublicOptions{DevCertHash: devCertHash(tlsConf), QUICPort: quicPort(cfg.QUICConfig.Addr)})
			})
		}
		return g.Wait()
	},
}

// buildOrigins parses WEBTRANSPORT_ORIGINS. With "directory" it also
// returns the fetcher of ORIGINS_URL (to be run), which is then required.
func buildOrigins(ctx context.Context, q config.QUICConfig) (*origins.Fetcher, *origins.Policy, error) {
	var f *origins.Fetcher
	if origins.UsesDirectory(q.WebTransportOrigins) {
		u := strings.TrimSpace(q.OriginsURL)
		if u == "" {
			return nil, nil, errors.New("WEBTRANSPORT_ORIGINS says directory: set ORIGINS_URL (the auth service's /v1/web-origins, e.g. http://auth:8080/v1/web-origins)")
		}
		f = origins.NewFetcher(u, q.OriginsRefresh, nil)
		logger.FromCtx(ctx).Info().Str("url", u).Dur("refresh", q.OriginsRefresh).
			Msg("web origins: following the directory's Boson web clients")
	}
	p, err := origins.Parse(q.WebTransportOrigins, f)
	if err != nil {
		logger.FromCtx(ctx).Warn().Err(err).Msg("WEBTRANSPORT_ORIGINS: entry ignored")
		p = origins.MustParse(q.WebTransportOrigins, f)
	}
	return f, p, nil
}

// RelayOptions maps the config to relay limits.
func RelayOptions(cfg config.Config, m *metrics.Metrics) relay.Options {
	rc := cfg.RelayConfig
	return relay.Options{
		MaxParticipants:         cfg.RoomConfig.MaxParticipants,
		MaxTracksPerSession:     rc.MaxTracksPerSession,
		MaxLayers:               rc.MaxLayers,
		MaxTrackBitrate:         rc.MaxTrackBitrate,
		IngressSlack:            rc.IngressSlack,
		GroupCacheBytes:         rc.GroupCacheBytes,
		SubQueueBytes:           rc.SubQueueBytes,
		KeyframeRequestInterval: rc.KeyframeRequestInterval,
		InitialBitrate:          rc.InitialBitrate,
		MinBitrate:              rc.MinBitrate,
		BacklogDown:             rc.BacklogDown,
		QueueDelayDown:          rc.QueueDelayDown,
		UpHold:                  rc.UpHold,
		StartupHold:             rc.StartupHold,
		MaxHold:                 rc.MaxHold,
		LayerIdleTimeout:        rc.LayerIdleTimeout,
		Metrics:                 m,
	}
}

func shutdownMetrics(ctx context.Context, p *metrics.Provider) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(sctx); err != nil {
		logger.FromCtx(ctx).Warn().Err(err).Msg("metrics shutdown")
	}
}

// BuildAuth picks the token verifier(s) from config.
func BuildAuth(ctx context.Context, a config.AuthConfig) (auth.AuthServiceImpl, error) {
	if a.Insecure {
		return auth.NewInsecureAuthService(), nil
	}
	comp := &auth.CompositeAuthService{}
	if a.JWTSecret != "" {
		comp.HMAC = auth.NewJWTAuthService(a.JWTSecret)
	}
	if a.JWKSURL != "" {
		j, err := auth.NewJWKSAuthService(ctx, a.JWKSURL, a.Issuer, a.Audience)
		if err != nil {
			return nil, err
		}
		comp.JWKS = j
	}
	if comp.HMAC == nil && comp.JWKS == nil {
		return nil, errors.New("set AUTH_JWKS_URL and/or JWT_SECRET, or AUTH_INSECURE=true for testing")
	}
	return comp, nil
}

// devCertHash is the hex SHA-256 of a self-signed dev certificate ("" for
// file-based, real certificates).
func devCertHash(c *tls.Config) string {
	if c == nil || len(c.Certificates) == 0 || len(c.Certificates[0].Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificates[0].Certificate[0])
	return hex.EncodeToString(sum[:])
}

func quicPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// BuildTLS loads the cert files, or makes a self-signed dev cert.
func BuildTLS(q config.QUICConfig) (*tls.Config, error) {
	if q.CertFile != "" {
		src, err := certs.NewFileSource(q.CertFile, q.KeyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{GetCertificate: src.GetCertificate}, nil
	}
	c, err := certs.SelfSigned(q.DevHostname)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{c}}, nil
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
