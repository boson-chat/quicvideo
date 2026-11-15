package transport_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/config"
	"github.com/boson-chat/quicvideo/server/internal/certs"
	"github.com/boson-chat/quicvideo/server/internal/relay"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/boson-chat/quicvideo/server/internal/transport"
	"github.com/boson-chat/quicvideo/server/metrics"
)

func startServer(t *testing.T, a auth.AuthServiceImpl) string {
	t.Helper()
	return startServerCfg(t, a, nil)
}

func startServerCfg(t *testing.T, a auth.AuthServiceImpl, edit func(*config.Config)) string {
	t.Helper()
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&cfg)
	}
	cfg.QUICConfig.Addr = "127.0.0.1:0"
	cert, err := certs.SelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := metrics.Nop()
	r := relay.New(ctx, relay.Options{Metrics: m, KeyframeRequestInterval: 50 * time.Millisecond})
	srv := transport.NewServer(cfg, &tls.Config{Certificates: []tls.Certificate{cert}}, a, r, m)
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return srv.Addr().String()
}

type recv struct {
	mu     sync.Mutex
	frames []client.RemoteFrame
	ch     chan client.RemoteFrame
}

func newRecv() *recv { return &recv{ch: make(chan client.RemoteFrame, 1024)} }

func (r *recv) on(f client.RemoteFrame) {
	r.mu.Lock()
	r.frames = append(r.frames, f)
	r.mu.Unlock()
	r.ch <- f
}

func (r *recv) next(t *testing.T) client.RemoteFrame {
	t.Helper()
	select {
	case f := <-r.ch:
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("no frame")
		return client.RemoteFrame{}
	}
}

func dial(t *testing.T, addr, token string, onFrame func(client.RemoteFrame)) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, addr, client.Options{Token: token, TLS: &tls.Config{InsecureSkipVerify: true}, OnFrame: onFrame})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitEvent(t *testing.T, c *client.Client, pred func(*controlpb.ControlMessage) bool) *controlpb.ControlMessage {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-c.Events():
			if !ok {
				t.Fatal("events closed")
			}
			if pred(m) {
				return m
			}
		case <-deadline:
			t.Fatal("event not received")
		}
	}
}

func payload(tag byte, n int) []byte { return bytes.Repeat([]byte{tag}, n) }

func waitActive(t *testing.T, track *client.LocalTrack, layer uint32, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for track.LayerActive(layer) != want {
		if time.Now().After(deadline) {
			t.Fatalf("layer %d active=%v never reported", layer, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitKeyframeRequest(t *testing.T, track *client.LocalTrack, layer uint32) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-track.KeyframeRequests():
			if l == layer {
				return
			}
		case <-deadline:
			t.Fatalf("no keyframe request for layer %d", layer)
		}
	}
}

func TestPublishSubscribeEndToEnd(t *testing.T) {
	addr := startServer(t, auth.NewInsecureAuthService())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice := dial(t, addr, "alice", nil)
	if _, err := alice.Join(ctx, "call"); err != nil {
		t.Fatal(err)
	}
	track, err := alice.Publish(ctx, controlpb.TrackKind_CAMERA, "h264", []*controlpb.Layer{
		{Id: 0, Width: 320, Height: 180, Fps: 30, Bitrate: 200_000},
		{Id: 1, Width: 1280, Height: 720, Fps: 30, Bitrate: 1_500_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nobody watches yet: frames are refused so the app can skip encoding.
	if err := track.WriteFrame(0, true, 0, payload('K', 10)); !errors.Is(err, client.ErrLayerInactive) {
		t.Fatalf("want ErrLayerInactive, got %v", err)
	}

	rx := newRecv()
	bob := dial(t, addr, "bob", rx.on)
	state, err := bob.Join(ctx, "call")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.GetTracks()) != 1 || state.GetTracks()[0].GetTrackId() != track.ID() || len(state.GetParticipants()) != 1 {
		t.Fatalf("room state %v", state)
	}
	waitEvent(t, alice, func(m *controlpb.ControlMessage) bool { return m.GetParticipantJoined() != nil })

	must(t, bob.Subscribe(track.ID(), 0, 0)) // max layer 0
	st := waitEvent(t, bob, func(m *controlpb.ControlMessage) bool { return m.GetSubscriptionState() != nil }).GetSubscriptionState()
	if st.GetTrackId() != track.ID() || st.GetTargetLayer() != 0 || st.GetMaxLayer() != 0 || st.GetForwarding() {
		t.Fatalf("state %v", st)
	}
	// Layer 0 becomes active (layer 1 stays off) and a keyframe is asked for.
	waitActive(t, track, 0, true)
	waitKeyframeRequest(t, track, 0)
	if track.LayerActive(1) {
		t.Fatal("layer 1 should be inactive")
	}

	must(t, track.WriteFrame(0, true, 0, payload('K', 2000)))
	must(t, track.WriteFrame(0, false, 0, payload('p', 100)))
	f := rx.next(t)
	if f.Layer != 0 || !f.GroupStart || !f.Keyframe || f.Payload[0] != 'K' || len(f.Payload) != 2000 {
		t.Fatalf("first frame layer %d", f.Layer)
	}
	if f = rx.next(t); f.Payload[0] != 'p' || f.GroupStart {
		t.Fatalf("second frame %q", f.Payload[:1])
	}
	if lat := time.Since(time.UnixMicro(int64(f.CaptureUS))); lat > 2*time.Second {
		t.Fatalf("latency %v", lat)
	}
	waitEvent(t, bob, func(m *controlpb.ControlMessage) bool { return m.GetSubscriptionState().GetForwarding() })

	// Raise the max to layer 1: it fits the estimate, so the server switches
	// up at layer 1's next keyframe (and activates + asks for it).
	must(t, bob.Subscribe(track.ID(), 1, 0))
	waitActive(t, track, 1, true)
	waitKeyframeRequest(t, track, 1)
	must(t, track.WriteFrame(1, true, 0, payload('H', 3000)))
	f = rx.next(t)
	for f.Layer == 0 { // layer 0 frames still in flight
		f = rx.next(t)
	}
	if f.Layer != 1 || !f.GroupStart || f.Payload[0] != 'H' {
		t.Fatalf("after switch: layer %d %q", f.Layer, f.Payload[:1])
	}
	must(t, track.WriteFrame(1, false, 0, payload('h', 10)))
	if f = rx.next(t); f.Layer != 1 || f.Payload[0] != 'h' {
		t.Fatalf("layer %d %q", f.Layer, f.Payload[:1])
	}
	waitEvent(t, bob, func(m *controlpb.ControlMessage) bool {
		s := m.GetSubscriptionState()
		return s != nil && s.GetLayer() == 1 && s.GetForwarding()
	})

	// Alice leaves: bob is told.
	_ = alice.Close()
	waitEvent(t, bob, func(m *controlpb.ControlMessage) bool { return m.GetTrackRemoved().GetTrackId() == track.ID() })
	waitEvent(t, bob, func(m *controlpb.ControlMessage) bool { return m.GetParticipantLeft() != nil })
}

func TestPublisherNeverBlocks(t *testing.T) {
	addr := startServer(t, auth.NewInsecureAuthService())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	alice := dial(t, addr, "alice", nil)
	_, err := alice.Join(ctx, "call")
	must(t, err)
	track, err := alice.Publish(ctx, controlpb.TrackKind_SCREEN, "h264", []*controlpb.Layer{{Id: 0, Bitrate: 1_000_000}},
		client.PublishOptions{MaxQueueBytes: 64 << 10, SendInactiveLayers: true})
	must(t, err)
	// 200 × 32 KB far exceeds the queue: writes must return at once and
	// drop until the next keyframe instead of blocking.
	start := time.Now()
	var backpressure int
	for i := range 200 {
		err := track.WriteFrame(0, i%50 == 0, 0, payload('x', 32<<10))
		if errors.Is(err, client.ErrBackpressure) {
			backpressure++
		}
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("WriteFrame blocked for %v", el)
	}
	st := track.Stats()[0]
	if backpressure == 0 || st.FramesDropped == 0 || st.KeyframesNeeded == 0 {
		t.Fatalf("no backpressure: %d errors, stats %+v", backpressure, st)
	}
}

func TestListenerCannotPublish(t *testing.T) {
	addr := startServer(t, auth.NewInsecureAuthService())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dial(t, addr, "carol@listener", nil)
	if _, err := c.Join(ctx, "call"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Publish(ctx, controlpb.TrackKind_SCREEN, "h264", []*controlpb.Layer{{Id: 0, Bitrate: 1000}})
	var se *client.ServerError
	if !errors.As(err, &se) || se.Code() != controlpb.Error_FORBIDDEN {
		t.Fatalf("want FORBIDDEN, got %v", err)
	}
}

func TestPublishBeforeJoin(t *testing.T) {
	addr := startServer(t, auth.NewInsecureAuthService())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dial(t, addr, "dave", nil)
	_, err := c.Publish(ctx, controlpb.TrackKind_SCREEN, "h264", []*controlpb.Layer{{Id: 0, Bitrate: 1000}})
	var se *client.ServerError
	if !errors.As(err, &se) || se.Code() != controlpb.Error_NOT_IN_ROOM {
		t.Fatalf("want NOT_IN_ROOM, got %v", err)
	}
}

func TestBadTokenRejected(t *testing.T) {
	addr := startServer(t, auth.NewJWTAuthService("secret"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Dial(ctx, addr, client.Options{Token: "nope", TLS: &tls.Config{InsecureSkipVerify: true}})
	var se *client.ServerError
	if !errors.As(err, &se) || se.Code() != controlpb.Error_UNAUTHENTICATED {
		t.Fatalf("want UNAUTHENTICATED, got %v", err)
	}
}

func TestRoomsClaimEnforced(t *testing.T) {
	addr := startServer(t, roomsAuth{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dial(t, addr, "eve", nil)
	_, err := c.Join(ctx, "other")
	var se *client.ServerError
	if !errors.As(err, &se) || se.Code() != controlpb.Error_FORBIDDEN {
		t.Fatalf("want FORBIDDEN, got %v", err)
	}
	if _, err := c.Join(ctx, "allowed"); err != nil {
		t.Fatal(err)
	}
}

type roomsAuth struct{}

func (roomsAuth) Verify(_ context.Context, tok string) (*auth.Identity, error) {
	return &auth.Identity{UserID: tok, DisplayName: tok, Role: auth.RoleSpeaker, Rooms: []string{"allowed"}}, nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRefreshAndExpiry(t *testing.T) {
	a := &expAuth{exp: map[string]time.Time{}}
	addr := startServerCfg(t, a, func(c *config.Config) { c.AuthConfig.TokenGrace = 300 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a.set("short", time.Now().Add(1*time.Second))
	a.set("long", time.Now().Add(time.Hour))
	a.set("other", time.Now().Add(time.Hour))
	c := dial(t, addr, "short", nil)
	if exp := c.TokenExpiresAt(); exp.IsZero() {
		t.Fatal("Welcome should carry the expiry")
	}
	_, err := c.Join(ctx, "call")
	must(t, err)
	var se *client.ServerError
	if err := c.Refresh(ctx, "other"); !errors.As(err, &se) || se.Code() != controlpb.Error_FORBIDDEN {
		t.Fatalf("refresh with another user's token: %v", err)
	}
	must(t, c.Refresh(ctx, "long"))
	if time.Until(c.TokenExpiresAt()) < 50*time.Minute {
		t.Fatalf("expiry not updated: %v", c.TokenExpiresAt())
	}
	// Still connected past the old expiry + grace.
	select {
	case <-c.Done():
		t.Fatal("closed despite refresh")
	case <-time.After(1800 * time.Millisecond):
	}

	// Without a refresh: warned, then closed after the grace period.
	a.set("short2", time.Now().Add(500*time.Millisecond))
	d := dial(t, addr, "short2", nil)
	waitEvent(t, d, func(m *controlpb.ControlMessage) bool {
		return m.GetError().GetCode() == controlpb.Error_TOKEN_EXPIRED && !m.GetError().GetFatal()
	})
	select {
	case <-d.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expired session not closed")
	}

	// TokenSource renews automatically.
	a.set("auto", time.Now().Add(1*time.Second))
	a.set("auto2", time.Now().Add(time.Hour))
	e, err := client.Dial(ctx, addr, client.Options{Token: "auto", TLS: &tls.Config{InsecureSkipVerify: true},
		TokenSource: func(context.Context) (string, error) { return "auto2", nil }})
	must(t, err)
	defer e.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Until(e.TokenExpiresAt()) < 50*time.Minute {
		if time.Now().After(deadline) {
			t.Fatal("TokenSource refresh never happened")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// expAuth: token = user id, with a configurable expiry per token. Users
// are the token up to a trailing digit ("short2" → "short").
type expAuth struct {
	mu  sync.Mutex
	exp map[string]time.Time
}

func (a *expAuth) set(tok string, at time.Time) { a.mu.Lock(); a.exp[tok] = at; a.mu.Unlock() }

func (a *expAuth) Verify(_ context.Context, tok string) (*auth.Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.exp[tok]
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	if time.Now().After(exp) {
		return nil, auth.ErrTokenExpired
	}
	user := strings.TrimRight(tok, "0123456789")
	switch user {
	case "long":
		user = "short"
	}
	return &auth.Identity{UserID: user, DisplayName: user, Role: auth.RoleSpeaker, ExpiresAt: exp}, nil
}
