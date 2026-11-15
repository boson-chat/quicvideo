// Package http serves the internal health/metrics listener and the public
// listener. They are separate so /metrics is never exposed.
package http

import (
	"context"
	"errors"
	nethttp "net/http"
	"time"

	"github.com/boson-chat/quicvideo/server/internal/logger"
)

// NewRouter serves /healthz, /version and /metrics. Internal only.
func NewRouter(version string, metricsHandler nethttp.Handler) nethttp.Handler {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /version", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte(version))
	})
	mux.Handle("GET /metrics", metricsHandler)
	return mux
}

// NewPublicRouter serves GET /healthz.
func NewPublicRouter() nethttp.Handler {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	return mux
}

func healthz(w nethttp.ResponseWriter, _ *nethttp.Request) {
	_, _ = w.Write([]byte("ok"))
}

func StartServerWithContext(ctx context.Context, addr, version string, metricsHandler nethttp.Handler) error {
	return serve(ctx, "HTTP", &nethttp.Server{
		Addr: addr, Handler: NewRouter(version, metricsHandler), ReadHeaderTimeout: 5 * time.Second,
	})
}

func StartPublicServerWithContext(ctx context.Context, addr string) error {
	return serve(ctx, "public HTTP", &nethttp.Server{
		Addr:              addr,
		Handler:           NewPublicRouter(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	})
}

func serve(ctx context.Context, name string, srv *nethttp.Server) error {
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.FromCtx(ctx).Info().Str("addr", srv.Addr).Msg(name + " listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
		return err
	}
	return nil
}
