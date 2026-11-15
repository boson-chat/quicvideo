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
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/server/config"
	"github.com/boson-chat/quicvideo/server/internal/logger"
	"github.com/boson-chat/quicvideo/server/internal/relay"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
)

// Application error codes used when closing a connection.
const (
	CodeOK       quic.ApplicationErrorCode = 0
	CodeProtocol quic.ApplicationErrorCode = 1
	CodeAuth     quic.ApplicationErrorCode = 2
	CodeSlow     quic.ApplicationErrorCode = 3
	CodeShutdown quic.ApplicationErrorCode = 4
)

type Server struct {
	cfg     config.Config
	tls     *tls.Config
	auth    auth.AuthServiceImpl
	relay   *relay.Relay
	metrics *metrics.Metrics
	nextID  atomic.Uint32

	addr  atomic.Pointer[net.Addr]
	ready chan struct{}
}

func NewServer(cfg config.Config, tlsConf *tls.Config, a auth.AuthServiceImpl, r *relay.Relay, m *metrics.Metrics) *Server {
	tlsConf = tlsConf.Clone()
	tlsConf.NextProtos = []string{wire.ALPN}
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

	go func() {
		<-ctx.Done()
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
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn *quic.Conn) {
	s.metrics.ConnectionOpened()
	defer s.metrics.ConnectionClosed()

	l := logger.FromCtx(ctx).With().Str("remote", conn.RemoteAddr().String()).Logger()
	sctx, cancel := context.WithCancel(logger.WithLogger(conn.Context(), l))
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.CloseWithError(CodeShutdown, "server shutting down")
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
	return errors.As(err, &appErr) || errors.As(err, &idle) || errors.Is(err, context.Canceled)
}

func (s *Server) helloDeadline() time.Time {
	return time.Now().Add(s.cfg.QUICConfig.HelloTimeout)
}
