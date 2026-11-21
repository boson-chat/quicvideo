// Package transport runs the QUIC listener and one session per connection:
// the control stream state machine, incoming media streams (publishing)
// and outgoing media streams (opened by the relay's forwarders).
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/server/config"
	"github.com/boson-chat/quicvideo/server/internal/logger"
	"github.com/boson-chat/quicvideo/server/internal/relay"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// Application error codes used when closing a connection (QUIC
// application error / WebTransport session error).
const (
	CodeOK       uint32 = 0
	CodeProtocol uint32 = 1
	CodeAuth     uint32 = 2
	CodeSlow     uint32 = 3
	CodeShutdown uint32 = 4
)

// WebTransportPath is where browsers open their session
// (https://host:port/wt).
const WebTransportPath = "/wt"

type Server struct {
	cfg     config.Config
	tls     *tls.Config
	auth    auth.AuthServiceImpl
	relay   *relay.Relay
	metrics *metrics.Metrics
	nextID  atomic.Uint32

	addr  atomic.Pointer[net.Addr]
	ready chan struct{}
	wt    *webtransport.Server
}

func NewServer(cfg config.Config, tlsConf *tls.Config, a auth.AuthServiceImpl, r *relay.Relay, m *metrics.Metrics) *Server {
	tlsConf = tlsConf.Clone()
	// One port, two protocols: native quicvideo, and HTTP/3 for browsers'
	// WebTransport sessions.
	tlsConf.NextProtos = []string{wire.ALPN, http3.NextProtoH3}
	tlsConf.MinVersion = tls.VersionTLS13
	return &Server{cfg: cfg, tls: tlsConf, auth: a, relay: r, metrics: m, ready: make(chan struct{})}
}

// QUICConfig is the quic-go config shared by the server and (mirrored in)
// the client SDK.
func QUICConfig(c config.QUICConfig) *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:  c.MaxIdleTimeout,
		KeepAlivePeriod: c.KeepAlive,
		// One control stream per connection.
		MaxIncomingStreams:             2,
		MaxIncomingUniStreams:          c.MaxIncomingUniStreams,
		InitialStreamReceiveWindow:     1 << 20,
		MaxStreamReceiveWindow:         c.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     c.MaxConnectionReceiveWindow,
		// Required by WebTransport (HTTP/3 datagrams, reliable resets).
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
	}
}

// Addr is the bound UDP address once Serve is listening (tests use :0).
func (s *Server) Addr() net.Addr {
	<-s.ready
	return *s.addr.Load()
}

// Serve listens until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := quic.ListenAddr(s.cfg.QUICConfig.Addr, s.tls, QUICConfig(s.cfg.QUICConfig))
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.QUICConfig.Addr, err)
	}
	a := ln.Addr()
	s.addr.Store(&a)
	close(s.ready)
	log := logger.FromCtx(ctx)
	log.Info().Str("addr", a.String()).Msg("QUIC listening")

	mux := http.NewServeMux()
	mux.HandleFunc(WebTransportPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.checkOrigin(r) {
			// Upgrade refuses it too, but writes no status: an unwritten
			// response is a 200, which a browser takes as an open session.
			log.Debug().Str("origin", r.Header.Get("Origin")).Msg("webtransport origin not allowed")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		sess, err := s.wt.Upgrade(w, r)
		if err != nil {
			log.Debug().Err(err).Msg("webtransport upgrade failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		go s.handle(ctx, wtConn{sess})
	})
	s.wt = &webtransport.Server{H3: &http3.Server{Handler: mux}, CheckOrigin: s.checkOrigin}
	webtransport.ConfigureHTTP3Server(s.wt.H3)

	go func() {
		<-ctx.Done()
		_ = s.wt.Close()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, quic.ErrServerClosed) {
				return nil
			}
			return err
		}
		if conn.ConnectionState().TLS.NegotiatedProtocol == http3.NextProtoH3 {
			go func() { _ = s.wt.ServeQUICConn(conn) }()
			continue
		}
		go s.handle(ctx, quicConn{conn})
	}
}

// checkOrigin applies WEBTRANSPORT_ORIGINS ("*" = any). Sessions still
// need a token in Hello; origins only narrow which pages may try.
func (s *Server) checkOrigin(r *http.Request) bool {
	allowed := s.cfg.QUICConfig.WebTransportOrigins
	if allowed == "" || allowed == "*" {
		return true
	}
	origin := r.Header.Get("Origin")
	for _, o := range strings.Split(allowed, ",") {
		if strings.TrimSpace(o) == origin {
			return true
		}
	}
	return false
}

func (s *Server) handle(ctx context.Context, conn mconn) {
	s.metrics.ConnectionOpened()
	defer s.metrics.ConnectionClosed()

	l := logger.FromCtx(ctx).With().Str("remote", conn.RemoteAddr().String()).Str("transport", conn.Kind()).Logger()
	sctx, cancel := context.WithCancel(logger.WithLogger(conn.Context(), l))
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			conn.Close(CodeShutdown, "server shutting down")
		case <-sctx.Done():
		}
	}()

	sess := newSession(s, conn, s.nextID.Add(1))
	if err := sess.run(sctx); err != nil && !isClosed(err) {
		l.Debug().Err(err).Msg("session ended")
	}
}

func isClosed(err error) bool {
	var appErr *quic.ApplicationError
	var idle *quic.IdleTimeoutError
	var sessErr *webtransport.SessionError
	return errors.As(err, &appErr) || errors.As(err, &idle) || errors.As(err, &sessErr) || errors.Is(err, context.Canceled)
}

func (s *Server) helloDeadline() time.Time {
	return time.Now().Add(s.cfg.QUICConfig.HelloTimeout)
}
