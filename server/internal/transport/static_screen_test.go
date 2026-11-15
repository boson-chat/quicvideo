package transport_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/client/media"
	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
)

// Regression: on a static screen, a newly watched layer's first keyframe is
// re-encoded from the last capture. It used to carry that capture's time
// (seconds old), so the relay's ingress health took the layer for a
// publisher that can't deliver on time and benched it for 15 s: a pinned
// screen share stayed on its low layer.
func TestStaticScreenUpSwitch(t *testing.T) {
	addr := startServer(t, auth.NewInsecureAuthService())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	alice := dial(t, addr, "alice", nil)
	_, err := alice.Join(ctx, "call")
	must(t, err)
	layers := []*controlpb.Layer{
		{Id: 0, Width: 1280, Height: 830, Fps: 5, Bitrate: 370_000},
		{Id: 1, Width: 1920, Height: 1246, Fps: 15, Bitrate: 1_900_000},
	}
	pub, _, err := media.PublishTrack(ctx, alice, controlpb.TrackKind_SCREEN, layers, passthroughEncoder, media.PublisherOptions{})
	must(t, err)
	defer pub.Close()

	bob := dial(t, addr, "bob", nil)
	st, err := bob.Join(ctx, "call")
	must(t, err)
	id := st.GetTracks()[0].GetTrackId()
	must(t, bob.Subscribe(id, 0, 0)) // small tile: low layer only
	pic := &stillPicture{}
	pub.Push(media.CapturedFrame{Picture: pic, CaptureUS: uint64(time.Now().UnixMicro())}) // the screen changes once
	waitState(t, bob, func(s *controlpb.SubscriptionState) bool { return s.GetLayer() == 0 && s.GetForwarding() })

	time.Sleep(3 * time.Second) // ... then stays static (ScreenCaptureKit sends nothing)

	must(t, bob.Subscribe(id, 1, 0)) // tile pinned: wants the top layer
	start := time.Now()
	waitState(t, bob, func(s *controlpb.SubscriptionState) bool { return s.GetLayer() == 1 && s.GetForwarding() })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("up-switch took %v", d)
	}
	// And it stays there: the layer must not be judged late and benched.
	// (On an overloaded test machine the bandwidth controller can still step
	// down for real congestion; that shows as a lower estimate and isn't
	// this bug, whose signature is a fallback with the estimate untouched.)
	time.Sleep(2 * time.Second)
	if s := bob.SubscriptionState(id); s.GetLayer() != 1 || s.GetTargetLayer() != 1 {
		if s.GetEstimateBps() < 2_500_000 {
			t.Logf("stepped down for congestion (estimate %d): machine under load, not the ingress-health bug", s.GetEstimateBps())
			return
		}
		t.Fatalf("fell back after the switch with the estimate untouched (layer benched): %v", s)
	}
}

func waitState(t *testing.T, c interface {
	SubscriptionState(uint32) *controlpb.SubscriptionState
}, ok func(*controlpb.SubscriptionState) bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		for id := uint32(1); id < 4; id++ {
			if s := c.SubscriptionState(id); s != nil && ok(s) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription state not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type stillPicture struct{}

func (stillPicture) Size() (int, int) { return 1920, 1246 }
func (stillPicture) Retain()          {}
func (stillPicture) Release()         {}

// passthroughEncoder "encodes" by emitting a frame per call (keyframe when
// forced or first) carrying the capture time it was given.
func passthroughEncoder(_ media.EncoderConfig, out func(media.EncodedFrame)) (media.Encoder, error) {
	return &ptEnc{out: out}, nil
}

type ptEnc struct {
	mu  sync.Mutex
	out func(media.EncodedFrame)
	n   int
}

func (e *ptEnc) Encode(_ media.Picture, us uint64, force bool) error {
	e.mu.Lock()
	key := force || e.n == 0
	e.n++
	e.mu.Unlock()
	e.out(media.EncodedFrame{Data: []byte{0, 0, 0, 1, 9, 0xf0}, Keyframe: key, CaptureUS: us})
	return nil
}

func (e *ptEnc) Close() {}
