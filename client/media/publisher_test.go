package media

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

type fakePic struct{ refs atomic.Int32 }

func (p *fakePic) Size() (int, int) { return 1280, 720 }
func (p *fakePic) Retain()          { p.refs.Add(1) }
func (p *fakePic) Release()         { p.refs.Add(-1) }

type encCall struct {
	us    uint64
	force bool
}

type fakeEnc struct {
	cfg    EncoderConfig
	out    func(EncodedFrame)
	mu     sync.Mutex
	calls  []encCall
	closed bool
	n      int
}

func (e *fakeEnc) Encode(p Picture, us uint64, force bool) error {
	e.mu.Lock()
	e.calls = append(e.calls, encCall{us, force})
	key := force || e.n == 0
	e.n++
	e.mu.Unlock()
	e.out(EncodedFrame{Data: []byte{1}, Keyframe: key, CaptureUS: us})
	return nil
}

func (e *fakeEnc) Close() { e.mu.Lock(); e.closed = true; e.mu.Unlock() }

func (e *fakeEnc) forced() (n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.calls {
		if c.force {
			n++
		}
	}
	return n
}

type write struct {
	layer uint32
	key   bool
}

type fakeTrack struct {
	mu     sync.Mutex
	writes []write
	err    func(layer uint32, key bool) error
}

func (t *fakeTrack) WriteFrame(layer uint32, key bool, _ uint64, _ []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writes = append(t.writes, write{layer, key})
	if t.err != nil {
		return t.err(layer, key)
	}
	return nil
}

type rig struct {
	p     *Publisher
	track *fakeTrack
	mu    sync.Mutex
	encs  map[uint32]*fakeEnc
	us    uint64
	pic   *fakePic
}

func newRig(t *testing.T) *rig {
	r := &rig{track: &fakeTrack{}, encs: map[uint32]*fakeEnc{}, us: 1_000_000, pic: &fakePic{}}
	r.pic.Retain() // the capture's own reference
	layers := CameraLayers(1280, 720, 30)
	byID := map[int]uint32{}
	for _, l := range layers {
		byID[int(l.GetHeight())] = l.GetId()
	}
	r.p = NewPublisher(r.track, layers, func(cfg EncoderConfig, out func(EncodedFrame)) (Encoder, error) {
		e := &fakeEnc{cfg: cfg, out: out}
		r.mu.Lock()
		r.encs[byID[cfg.Height]] = e
		r.mu.Unlock()
		return e, nil
	}, PublisherOptions{Now: func() time.Time { return time.UnixMicro(int64(r.us)) }})
	return r
}

func (r *rig) enc(l uint32) *fakeEnc { r.mu.Lock(); defer r.mu.Unlock(); return r.encs[l] }

// push sends n frames at 30 fps.
func (r *rig) push(n int) {
	for range n {
		r.us += 33_333
		r.p.Push(CapturedFrame{Picture: r.pic, CaptureUS: r.us})
	}
}

func TestPublisherEncodesOnlyActiveLayers(t *testing.T) {
	r := newRig(t)
	r.push(5)
	if len(r.encs) != 0 || len(r.track.writes) != 0 {
		t.Fatal("nothing should be encoded before any layer is active")
	}
	r.p.SetActive(1, true)
	// Activation re-encodes the last picture as a keyframe immediately.
	if e := r.enc(1); e == nil || len(e.calls) != 1 || !e.calls[0].force {
		t.Fatalf("activation should force a keyframe from the last picture: %+v", e)
	}
	r.push(30)
	if e := r.enc(1); len(e.calls) != 31 || e.forced() != 1 {
		t.Fatalf("layer 1 (30 fps): %d encodes, %d forced", len(e.calls), e.forced())
	}
	if r.enc(0) != nil || r.enc(2) != nil {
		t.Fatal("inactive layers must have no encoder")
	}
	if c := r.enc(1).cfg; c.Width != 640 || c.Height != 360 || c.FPS != 30 || c.GOP != 2*time.Second {
		t.Fatalf("config %+v", c)
	}
	r.p.SetActive(1, false)
	if !r.enc(1).closed || len(r.p.ActiveLayers()) != 0 {
		t.Fatal("deactivated encoder should be closed")
	}
}

// A keyframe re-encoded for a static screen is stamped with the current
// time, not the (old) time of the last capture.
func TestPublisherKickUsesCurrentTime(t *testing.T) {
	r := newRig(t)
	r.push(1)         // captured at t0
	r.us += 5_000_000 // 5 s of static screen: no captures
	r.p.SetActive(2, true)
	e := r.enc(2)
	if len(e.calls) != 1 || e.calls[0].us < r.us {
		t.Fatalf("kicked keyframe stamped %d, want ≥ now %d", e.calls[0].us, r.us)
	}
}

func TestPublisherFPSCap(t *testing.T) {
	r := newRig(t)
	r.p.SetActive(0, true) // 15 fps layer
	r.push(30)             // 1 s at 30 fps
	if n := len(r.enc(0).calls); n < 15 || n > 17 {
		t.Fatalf("15 fps layer got %d encodes for 30 captures", n)
	}
}

func TestPublisherKeyframeForcing(t *testing.T) {
	r := newRig(t)
	r.p.SetActive(2, true)
	r.push(10)
	e := r.enc(2)
	before := e.forced()
	// Server/send queue asks: the last picture is re-encoded as a keyframe
	// right away (a static screen might not send another one).
	r.p.KeyframeNeeded(2)
	if e.forced() != before+1 || !e.calls[len(e.calls)-1].force {
		t.Fatal("KeyframeNeeded should force an immediate keyframe")
	}
	// The track refuses a frame (needs a keyframe): the next one is forced.
	var once atomic.Bool
	r.track.err = func(_ uint32, key bool) error {
		if !key && once.CompareAndSwap(false, true) {
			return client.ErrNeedKeyframe
		}
		return nil
	}
	r.push(1)
	r.push(1)
	if !e.calls[len(e.calls)-1].force {
		t.Fatal("after ErrNeedKeyframe the next encode must be a keyframe")
	}
	// Backpressure behaves the same.
	r.track.err = func(_ uint32, key bool) error {
		if !key {
			return client.ErrBackpressure
		}
		return nil
	}
	n := e.forced()
	r.push(1)
	r.push(1)
	if e.forced() <= n {
		t.Fatal("after ErrBackpressure the next encode must be a keyframe")
	}
	// KeyframeNeeded for an inactive layer is ignored.
	r.p.KeyframeNeeded(0)
	if r.enc(0) != nil {
		t.Fatal("inactive layer got an encoder")
	}
}

func TestPublisherReleasesPictures(t *testing.T) {
	r := newRig(t)
	r.p.SetActive(0, true)
	r.p.SetActive(2, true)
	r.push(20)
	r.p.Close()
	if refs := r.pic.refs.Load(); refs != 1 {
		t.Fatalf("picture refcount %d after Close, want 1 (the capture's own)", refs)
	}
	r.push(1) // after Close: ignored, no leak
	if refs := r.pic.refs.Load(); refs != 1 {
		t.Fatalf("refcount %d", refs)
	}
}

func TestPublisherEncoderErrors(t *testing.T) {
	var errs atomic.Int32
	p := NewPublisher(&fakeTrack{}, ScreenLayers(1920, 1080), func(EncoderConfig, func(EncodedFrame)) (Encoder, error) {
		return nil, errors.New("no hardware encoder")
	}, PublisherOptions{Screen: true, OnError: func(uint32, error) { errs.Add(1) }})
	p.SetActive(0, true)
	if errs.Load() != 1 || len(p.ActiveLayers()) != 0 {
		t.Fatal("factory error should be reported and leave no encoder")
	}
	if p.opts.GOP != 4*time.Second {
		t.Fatalf("screen GOP %v", p.opts.GOP)
	}
}

var _ TrackWriter = (*client.LocalTrack)(nil)
var _ = controlpb.TrackKind_CAMERA
