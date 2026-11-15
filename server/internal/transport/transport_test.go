package transport_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
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
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
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

	if err := bob.Subscribe(track.ID(), 0, 0); err != nil {
		t.Fatal(err)
	}
	// No cache yet: the server asks alice for a keyframe on layer 0.
	select {
	case l := <-track.KeyframeRequests():
		if l != 0 {
			t.Fatalf("keyframe request for layer %d", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no keyframe request")
	}

	must(t, track.WriteFrame(1, true, 0, payload('H', 3000)))
	must(t, track.WriteFrame(0, true, 0, payload('K', 2000)))
	must(t, track.WriteFrame(0, false, 0, payload('p', 100)))
	f := rx.next(t)
	if f.Layer != 0 || !f.GroupStart || !f.Keyframe || f.Payload[0] != 'K' || len(f.Payload) != 2000 {
		t.Fatalf("first frame %+v", f.Layer)
	}
	if f = rx.next(t); f.Payload[0] != 'p' || f.GroupStart {
		t.Fatalf("second frame %q", f.Payload[:1])
	}
	if lat := time.Since(time.UnixMicro(int64(f.CaptureUS))); lat > 2*time.Second {
		t.Fatalf("latency %v", lat)
	}

	// Switch to layer 1: happens at layer 1's next keyframe (here: its
	// cached group replays immediately).
	must(t, bob.Subscribe(track.ID(), 1, 0))
	f = rx.next(t)
	if f.Layer != 1 || !f.GroupStart || f.Payload[0] != 'H' {
		t.Fatalf("after switch: layer %d %q", f.Layer, f.Payload[:1])
	}
	must(t, track.WriteFrame(0, false, 0, payload('x', 10))) // old layer: not forwarded
	must(t, track.WriteFrame(1, false, 0, payload('h', 10)))
	if f = rx.next(t); f.Layer != 1 || f.Payload[0] != 'h' {
		t.Fatalf("layer %d %q", f.Layer, f.Payload[:1])
	}

	// Alice leaves: bob is told.
	_ = alice.Close()
	waitEvent(t, bob, func(m *controlpb.ControlMessage) bool { return m.GetTrackRemoved().GetTrackId() == track.ID() })
	waitEvent(t, bob, func(m *controlpb.ControlMessage) bool { return m.GetParticipantLeft() != nil })
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
