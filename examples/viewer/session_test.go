package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/client/media"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

type fakePic struct{}

func (fakePic) Size() (int, int) { return 1280, 720 }
func (fakePic) Retain()          {}
func (fakePic) Release()         {}

// fakeSource pushes frames every 5 ms until stopped or silenced; it can
// also end on its own like a camera that was unplugged.
type fakeSource struct {
	push    func(media.CapturedFrame)
	stops   atomic.Int32
	silent  atomic.Bool
	delay   time.Duration // before the first frame
	ended   chan error
	quit    chan struct{}
	stopped sync.Once
}

func newFakeSource(push func(media.CapturedFrame), delay time.Duration) *fakeSource {
	f := &fakeSource{push: push, delay: delay, ended: make(chan error, 1), quit: make(chan struct{})}
	go func() {
		select {
		case <-time.After(delay):
		case <-f.quit:
			return
		}
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-f.quit:
				return
			case <-t.C:
				if !f.silent.Load() {
					f.push(media.CapturedFrame{Picture: fakePic{}, CaptureUS: uint64(time.Now().UnixMicro())})
				}
			}
		}
	}()
	return f
}

func (f *fakeSource) Size() (int, int)             { return 0, 0 }
func (f *fakeSource) FPS() int                     { return 30 }
func (f *fakeSource) Ended() <-chan error          { return f.ended }
func (f *fakeSource) Stop()                        { f.stops.Add(1); f.stopped.Do(func() { close(f.quit) }) }
func (f *fakeSource) running() bool                { return f.stops.Load() == 0 }
func (f *fakeSource) fail(err error)               { f.ended <- err }
func (f *fakeSource) stopCount() int32             { return f.stops.Load() }
func (f *fakeSource) waitStopped() <-chan struct{} { return f.quit }

type fakePub struct {
	pushes atomic.Int32
	closes atomic.Int32
}

func (p *fakePub) Push(media.CapturedFrame) { p.pushes.Add(1) }
func (p *fakePub) Close()                   { p.closes.Add(1) }

type rig struct {
	t    *testing.T
	s    *session
	mu   sync.Mutex
	srcs []*fakeSource
	pubs []*fakePub
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, s: newSession(func() {})}
	r.s.stallAfter = 200 * time.Millisecond
	r.s.publish = func(context.Context, controlpb.TrackKind, []*controlpb.Layer) (publication, error) {
		p := &fakePub{}
		r.mu.Lock()
		r.pubs = append(r.pubs, p)
		r.mu.Unlock()
		return p, nil
	}
	return r
}

// start begins a camera share whose first frame comes after delay; it
// returns once the source exists.
func (r *rig) start(kind controlpb.TrackKind, delay time.Duration) *fakeSource {
	made := make(chan *fakeSource, 1)
	go r.s.startShare(context.Background(), kind, "cam", func(push func(media.CapturedFrame)) (media.Source, error) {
		f := newFakeSource(push, delay)
		r.mu.Lock()
		r.srcs = append(r.srcs, f)
		r.mu.Unlock()
		made <- f
		return f, nil
	})
	return <-made
}

func (r *rig) pub(i int) *fakePub {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.pubs) {
		return nil
	}
	return r.pubs[i]
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) camera() string { c, _ := r.s.sharing(); return c }

// live waits for a share to be published and frames to flow.
func (r *rig) live(i int) *fakePub {
	r.t.Helper()
	eventually(r.t, "share published", func() bool { p := r.pub(i); return p != nil && p.pushes.Load() > 3 })
	return r.pub(i)
}

// Every way out of a share releases the device once and unpublishes.
func TestShareReleasedOnEveryExit(t *testing.T) {
	for name, exit := range map[string]func(r *rig, src *fakeSource){
		"camera off": func(r *rig, _ *fakeSource) { r.s.StopCamera() },
		"leave":      func(r *rig, _ *fakeSource) { r.s.Close() },
		"leave twice, concurrently": func(r *rig, _ *fakeSource) {
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() { defer wg.Done(); r.s.Close() }()
			}
			wg.Wait()
		},
		"camera off then leave": func(r *rig, _ *fakeSource) {
			go r.s.StopCamera()
			r.s.Close()
		},
		"device error": func(r *rig, src *fakeSource) { src.fail(errors.New("camera was disconnected")) },
		"no pictures":  func(r *rig, src *fakeSource) { src.silent.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			src := r.start(controlpb.TrackKind_CAMERA, 0)
			pub := r.live(0)
			if r.camera() == "" {
				t.Fatal("UI should show the camera on")
			}
			exit(r, src)
			eventually(t, "device released", func() bool { return src.stopCount() == 1 })
			eventually(t, "track unpublished", func() bool { return pub.closes.Load() == 1 })
			eventually(t, "UI shows camera off", func() bool { return r.camera() == "" })
			r.s.mu.Lock()
			preview := r.s.preview
			r.s.mu.Unlock()
			if preview != nil {
				t.Fatal("self-preview still shown")
			}
			time.Sleep(50 * time.Millisecond)
			if src.stopCount() != 1 || pub.closes.Load() != 1 {
				t.Fatalf("stopped %d times, unpublished %d times", src.stopCount(), pub.closes.Load())
			}
		})
	}
}

// (a) Capture failing mid-stream turns the camera off for everyone: the
// track is unpublished (no frozen tile for the others) and the status says why.
func TestCaptureErrorMidStreamUnpublishes(t *testing.T) {
	r := newRig(t)
	src := r.start(controlpb.TrackKind_CAMERA, 0)
	pub := r.live(0)
	src.fail(errors.New("media: camera error: device busy"))
	eventually(t, "unpublished", func() bool { return pub.closes.Load() == 1 })
	eventually(t, "camera off", func() bool { return r.camera() == "" })
	r.s.mu.Lock()
	status := r.s.status
	r.s.mu.Unlock()
	if !strings.Contains(status, "device busy") {
		t.Fatalf("status %q doesn't say why", status)
	}
	// It can be turned on again.
	r.start(controlpb.TrackKind_CAMERA, 0)
	r.live(1)
	if r.camera() == "" {
		t.Fatal("restart after a failure didn't share")
	}
}

// A screen that sends no frames (nothing changed on it) is not "stalled".
func TestIdleScreenIsNotStalled(t *testing.T) {
	r := newRig(t)
	src := r.start(controlpb.TrackKind_SCREEN, 0)
	pub := r.live(0)
	src.silent.Store(true)
	time.Sleep(4 * r.s.stallAfter)
	if pub.closes.Load() != 0 || src.stopCount() != 0 {
		t.Fatal("an idle screen share was stopped")
	}
	r.s.Close()
	eventually(t, "screen released", func() bool { return src.stopCount() == 1 && pub.closes.Load() == 1 })
}

// Stopping while the camera is still starting (no first frame yet) wins:
// the device is released and nothing gets published later.
func TestStopWhileStarting(t *testing.T) {
	for name, stop := range map[string]func(*session){
		"camera off": (*session).StopCamera,
		"leave":      (*session).Close,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			src := r.start(controlpb.TrackKind_CAMERA, 300*time.Millisecond)
			if r.camera() == "" {
				eventually(t, "UI shows the camera starting", func() bool { return r.camera() != "" })
			}
			stop(r.s)
			eventually(t, "device released", func() bool { return src.stopCount() == 1 })
			time.Sleep(500 * time.Millisecond) // past the first frame
			if p := r.pub(0); p != nil {
				t.Fatal("published after being stopped")
			}
			if r.camera() != "" {
				t.Fatal("UI shows the camera on after stopping it")
			}
		})
	}
}

// Clicking camera on twice: one share, the other device released.
func TestRestartWhileStartingKeepsOne(t *testing.T) {
	r := newRig(t)
	first := r.start(controlpb.TrackKind_CAMERA, 200*time.Millisecond)
	second := r.start(controlpb.TrackKind_CAMERA, 0)
	r.live(0)
	eventually(t, "first device released", func() bool { return first.stopCount() == 1 })
	time.Sleep(400 * time.Millisecond)
	if second.stopCount() != 0 || r.pub(1) != nil {
		t.Fatalf("second stopped=%d, extra publication=%v", second.stopCount(), r.pub(1) != nil)
	}
	r.s.Close()
	eventually(t, "released on leave", func() bool { return second.stopCount() == 1 })
}

// After Leave nothing can start (e.g. the initial -camera share racing quit).
func TestNoStartAfterClose(t *testing.T) {
	r := newRig(t)
	r.s.Close()
	called := false
	r.s.startShare(context.Background(), controlpb.TrackKind_CAMERA, "cam", func(func(media.CapturedFrame)) (media.Source, error) {
		called = true
		return nil, errors.New("unreachable")
	})
	if called {
		t.Fatal("a source was started after Close")
	}
}
