package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
)

// ---- fakes -----------------------------------------------------------------

type fakeWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	urgency  int8
	closed   bool
	canceled bool
	code     uint64
	gate     chan struct{} // non-nil: each Write waits for a token (or cancel)
	cancelCh chan struct{}
}

var errCanceled = errors.New("fake: canceled")

func (w *fakeWriter) Write(p []byte) (int, error) {
	if w.gate != nil {
		select {
		case <-w.gate:
		case <-w.cancelCh:
			return 0, errCanceled
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.canceled {
		return 0, errCanceled
	}
	return w.buf.Write(p)
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *fakeWriter) CancelWrite(code uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.canceled {
		w.canceled, w.code = true, code
		close(w.cancelCh)
	}
}

func (w *fakeWriter) state() (closed, canceled bool, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed, w.canceled, bytes.Clone(w.buf.Bytes())
}

type fakePeer struct {
	id      uint32
	gated   bool
	mu      sync.Mutex
	ctrl    []*controlpb.ControlMessage
	writers []*fakeWriter
	opened  chan *fakeWriter
}

func newPeer(id uint32) *fakePeer { return &fakePeer{id: id, opened: make(chan *fakeWriter, 64)} }

func (p *fakePeer) ID() uint32 { return p.id }

func (p *fakePeer) OpenGroup(_ context.Context, urgency int8) (GroupWriter, error) {
	w := &fakeWriter{urgency: urgency, cancelCh: make(chan struct{})}
	if p.gated {
		w.gate = make(chan struct{}, 1024)
	}
	p.mu.Lock()
	p.writers = append(p.writers, w)
	p.mu.Unlock()
	p.opened <- w
	return w, nil
}

func (p *fakePeer) SendControl(m *controlpb.ControlMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctrl = append(p.ctrl, m)
}

func (p *fakePeer) keyframeRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.ctrl {
		if m.GetKeyframeRequest() != nil {
			n++
		}
	}
	return n
}

func (p *fakePeer) has(pred func(*controlpb.ControlMessage) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.ctrl {
		if pred(m) {
			return true
		}
	}
	return false
}

func (p *fakePeer) nextWriter(t *testing.T) *fakeWriter {
	t.Helper()
	select {
	case w := <-p.opened:
		return w
	case <-time.After(2 * time.Second):
		t.Fatal("no group stream opened")
		return nil
	}
}

func (p *fakePeer) noWriter(t *testing.T) {
	t.Helper()
	select {
	case <-p.opened:
		t.Fatal("unexpected group stream")
	case <-time.After(50 * time.Millisecond):
	}
}

// ---- helpers ---------------------------------------------------------------

type env struct {
	t      *testing.T
	r      *Relay
	m      *metrics.Metrics
	pubP   *fakePeer
	pub    *Member
	subP   *fakePeer
	sub    *Member
	track  uint32
	cancel context.CancelFunc
}

func newEnv(t *testing.T, opts Options, gated bool) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts.Metrics = metrics.Nop()
	opts.NoLoop = true
	if opts.InitialBitrate == 0 {
		opts.InitialBitrate = 1e9 // mechanics tests: the controller never limits
	}
	r := New(ctx, opts)
	e := &env{t: t, r: r, m: opts.Metrics, pubP: newPeer(1), subP: newPeer(2), cancel: cancel}
	e.subP.gated = gated
	var err error
	e.pub, _, err = r.Join(e.pubP, Participant{UserID: "alice", CanPublish: true}, "room")
	must(t, err)
	e.sub, _, err = r.Join(e.subP, Participant{UserID: "bob"}, "room")
	must(t, err)
	e.track, err = e.pub.Publish(&controlpb.Publish{
		Kind: controlpb.TrackKind_CAMERA, Codec: "h264",
		Layers: []*controlpb.Layer{{Id: 0, Bitrate: 300_000}, {Id: 1, Bitrate: 1_500_000}},
	})
	must(t, err)
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func frame(key bool, n int, tag byte) *wire.Frame {
	return &wire.Frame{Keyframe: key, CaptureUS: uint64(tag), Payload: bytes.Repeat([]byte{tag}, n)}
}

// group feeds frames as one incoming group and ends it.
func (e *env) group(layer uint32, g uint64, frames ...*wire.Frame) {
	e.t.Helper()
	in := e.open(layer, g)
	for _, f := range frames {
		must(e.t, in.Frame(f))
	}
	in.End(false)
}

func (e *env) open(layer uint32, g uint64) *GroupIngest {
	e.t.Helper()
	in, err := e.pub.OpenGroup(wire.GroupHeader{TrackID: e.track, Layer: layer, Group: g})
	must(e.t, err)
	return in
}

// read parses a writer's bytes once it is closed or canceled.
func read(t *testing.T, w *fakeWriter) (wire.GroupHeader, []*wire.Frame, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		closed, canceled, data := w.state()
		if closed || canceled {
			r, err := wire.NewMediaReader(bytes.NewReader(data), 0)
			if err != nil {
				return wire.GroupHeader{}, nil, canceled
			}
			var fs []*wire.Frame
			for {
				f, err := r.Next()
				if err != nil {
					if !errors.Is(err, io.EOF) && !canceled {
						t.Fatalf("read: %v", err)
					}
					break
				}
				fs = append(fs, f)
			}
			return r.Header(), fs, canceled
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never finished")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func tags(fs []*wire.Frame) []byte {
	var b []byte
	for _, f := range fs {
		b = append(b, f.Payload[0])
	}
	return b
}

// ---- tests -----------------------------------------------------------------

func TestForwardGroup(t *testing.T) {
	e := newEnv(t, Options{}, false)
	must(t, e.sub.Subscribe(e.track, 0, 0))
	e.group(0, 1, frame(true, 100, 'a'), frame(false, 10, 'b'), frame(false, 10, 'c'))
	w := e.subP.nextWriter(t)
	h, fs, canceled := read(t, w)
	if canceled || h.TrackID != e.track || h.Layer != 0 || h.Group != 1 || string(tags(fs)) != "abc" {
		t.Fatalf("got %+v %q canceled=%v", h, tags(fs), canceled)
	}
	if w.urgency != 4 {
		t.Fatalf("camera urgency %d, want 4", w.urgency)
	}
}

func TestCacheReplayOnSubscribe(t *testing.T) {
	e := newEnv(t, Options{}, false)
	in := e.open(0, 5)
	must(t, in.Frame(frame(true, 100, 'k')))
	must(t, in.Frame(frame(false, 10, 'p')))
	must(t, e.sub.Subscribe(e.track, 0, 3))
	must(t, in.Frame(frame(false, 10, 'q')))
	in.End(false)
	w := e.subP.nextWriter(t)
	h, fs, _ := read(t, w)
	if h.Group != 5 || string(tags(fs)) != "kpq" || w.urgency != 3 {
		t.Fatalf("got group %d %q urgency %d", h.Group, tags(fs), w.urgency)
	}
	if n := e.pubP.keyframeRequests(); n != 0 {
		t.Fatalf("cache hit should not request a keyframe, got %d", n)
	}
}

func TestNoCacheRequestsKeyframeRateLimited(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newEnv(t, Options{Now: clock}, false)
	must(t, e.sub.Subscribe(e.track, 1, 0))
	if n := e.pubP.keyframeRequests(); n != 1 {
		t.Fatalf("want 1 keyframe request, got %d", n)
	}
	// Joined mid-group on a layer with no cache: frames are dropped and
	// keep asking, but at most once per interval.
	in := e.open(1, 1)
	e.r.mu.Lock()
	ls := e.r.tracks[e.track].layers[1]
	e.r.mu.Unlock()
	ls.mu.Lock()
	ls.hasCurrent, ls.current, ls.cacheOK = true, 1, false
	ls.mu.Unlock()
	in.n = 1 // pretend the keyframe went by before we subscribed
	for range 5 {
		must(t, in.Frame(frame(false, 10, 'x')))
	}
	if n := e.pubP.keyframeRequests(); n != 1 {
		t.Fatalf("rate limit: want 1, got %d", n)
	}
	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	must(t, in.Frame(frame(false, 10, 'x')))
	if n := e.pubP.keyframeRequests(); n != 2 {
		t.Fatalf("after interval: want 2, got %d", n)
	}
	e.subP.noWriter(t)
}

func TestUpSwitchWaitsForNextKeyframe(t *testing.T) {
	e := newEnv(t, Options{}, false)
	must(t, e.sub.Subscribe(e.track, 0, 0))
	in0 := e.open(0, 1)
	in1 := e.open(1, 1)
	must(t, in0.Frame(frame(true, 50, 'A')))
	must(t, in1.Frame(frame(true, 50, '1')))
	w0 := e.subP.nextWriter(t)
	before := e.pubP.keyframeRequests()

	// Up to layer 1: no replay of its cached group (a burst while probing);
	// switch at its next keyframe, which the relay asks for.
	must(t, e.sub.Subscribe(e.track, 1, 0))
	if e.pubP.keyframeRequests() != before+1 {
		t.Fatal("up-switch should request a keyframe (unknown GOP)")
	}
	e.subP.noWriter(t)
	must(t, in1.Frame(frame(false, 5, '2'))) // mid-group: not forwarded
	must(t, in0.Frame(frame(false, 5, 'B'))) // still on layer 0 meanwhile
	in0.End(false)
	in1.End(false)
	_, fs0, _ := read(t, w0)
	e.group(1, 2, frame(true, 50, '3'), frame(false, 5, '4'))
	h1, fs1, _ := read(t, e.subP.nextWriter(t))
	if string(tags(fs0)) != "AB" || h1.Layer != 1 || h1.Group != 2 || string(tags(fs1)) != "34" {
		t.Fatalf("layer 0 %q, then layer %d group %d %q", tags(fs0), h1.Layer, h1.Group, tags(fs1))
	}
	if l, ok := e.sub.subs[e.track].Layer(); !ok || l != 1 {
		t.Fatalf("active layer %d %v", l, ok)
	}
}

func TestDownSwitchReplaysCache(t *testing.T) {
	e := newEnv(t, Options{}, false)
	must(t, e.sub.Subscribe(e.track, 1, 0))
	in0 := e.open(0, 1)
	in1 := e.open(1, 1)
	must(t, in0.Frame(frame(true, 50, 'a')))
	must(t, in1.Frame(frame(true, 50, 'H')))
	w1 := e.subP.nextWriter(t)
	_ = w1
	// Down (the client lowered its max): immediate, from layer 0's cache.
	must(t, e.sub.Subscribe(e.track, 0, 0))
	w0 := e.subP.nextWriter(t)
	must(t, in0.Frame(frame(false, 5, 'b')))
	in0.End(false)
	h, fs, _ := read(t, w0)
	if h.Layer != 0 || string(tags(fs)) != "ab" {
		t.Fatalf("got layer %d %q", h.Layer, tags(fs))
	}
}

func TestLayerSwitchWaitsForKeyframe(t *testing.T) {
	e := newEnv(t, Options{}, false)
	must(t, e.sub.Subscribe(e.track, 0, 0))
	in0 := e.open(0, 1)
	must(t, in0.Frame(frame(true, 50, 'A')))
	w0 := e.subP.nextWriter(t)
	must(t, e.sub.Subscribe(e.track, 1, 0))  // layer 1 has nothing yet
	must(t, in0.Frame(frame(false, 5, 'B'))) // still on layer 0 meanwhile
	in0.End(false)
	_, fs0, _ := read(t, w0) // let the old group drain (a backlog would be superseded)
	e.group(1, 1, frame(true, 50, '1'))
	h1, fs1, _ := read(t, e.subP.nextWriter(t))
	if string(tags(fs0)) != "AB" || h1.Layer != 1 || string(tags(fs1)) != "1" {
		t.Fatalf("got %q / layer %d %q", tags(fs0), h1.Layer, tags(fs1))
	}
}

func TestSupersededGroupIsCanceled(t *testing.T) {
	e := newEnv(t, Options{}, true) // subscriber writes block until released
	must(t, e.sub.Subscribe(e.track, 0, 0))
	in := e.open(0, 1)
	must(t, in.Frame(frame(true, 50, 'a')))
	w1 := e.subP.nextWriter(t)
	for range 4 {
		must(t, in.Frame(frame(false, 50, 'b'))) // backlog: subscriber is stuck
	}
	in.End(false)
	e.group(0, 2, frame(true, 50, 'c'), frame(false, 5, 'd'))
	_, canceled, _ := w1.state()
	if !canceled {
		t.Fatal("old group should be reset when a newer one starts")
	}
	w2 := e.subP.nextWriter(t)
	for range 10 {
		w2.gate <- struct{}{}
	}
	h, fs, _ := read(t, w2)
	if h.Group != 2 || string(tags(fs)) != "cd" {
		t.Fatalf("got group %d %q", h.Group, tags(fs))
	}
	if e.m.Snapshot().DroppedGroups["superseded"] != 1 {
		t.Fatalf("drops %+v", e.m.Snapshot().DroppedGroups)
	}
}

func TestOverflowDropsGroupAndRequestsKeyframe(t *testing.T) {
	e := newEnv(t, Options{SubQueueBytes: 1000}, true)
	in := e.open(0, 1)
	must(t, in.Frame(frame(true, 100, 'a')))
	must(t, e.sub.Subscribe(e.track, 0, 0))
	before := e.pubP.keyframeRequests()
	w1 := e.subP.nextWriter(t)
	for range 20 {
		_ = in.Frame(frame(false, 100, 'b'))
	}
	_, canceled, _ := w1.state()
	if !canceled {
		t.Fatal("overflowing group should be reset")
	}
	if e.pubP.keyframeRequests() <= before {
		t.Fatal("overflow should request a keyframe")
	}
	if e.m.Snapshot().DroppedGroups["overflow"] != 1 {
		t.Fatalf("drops %+v", e.m.Snapshot().DroppedGroups)
	}
	// Recovery at the next group.
	e.group(0, 2, frame(true, 100, 'k'))
	w2 := e.subP.nextWriter(t)
	w2.gate <- struct{}{}
	w2.gate <- struct{}{}
	w2.gate <- struct{}{}
	h, fs, _ := read(t, w2)
	if h.Group != 2 || string(tags(fs)) != "k" {
		t.Fatalf("got group %d %q", h.Group, tags(fs))
	}
}

func TestStaleGroup(t *testing.T) {
	e := newEnv(t, Options{}, false)
	e.group(0, 5, frame(true, 10, 'a'))
	if _, err := e.pub.OpenGroup(wire.GroupHeader{TrackID: e.track, Layer: 0, Group: 4}); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	// Old group still streaming when a newer one starts: its frames go stale.
	old := e.open(0, 6)
	newer := e.open(0, 7)
	must(t, old.Frame(frame(true, 10, 'x')))
	must(t, newer.Frame(frame(true, 10, 'y')))
	if err := old.Frame(frame(false, 10, 'z')); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
}

func TestOwnerLeaveRemovesTracks(t *testing.T) {
	e := newEnv(t, Options{}, false)
	must(t, e.sub.Subscribe(e.track, 0, 0))
	sub := e.sub.subs[e.track]
	e.pub.Leave("left")
	if !e.subP.has(func(m *controlpb.ControlMessage) bool { return m.GetTrackRemoved().GetTrackId() == e.track }) {
		t.Fatal("no TrackRemoved")
	}
	if !e.subP.has(func(m *controlpb.ControlMessage) bool { return m.GetParticipantLeft().GetSessionId() == 1 }) {
		t.Fatal("no ParticipantLeft")
	}
	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Fatal("forwarder still running")
	}
	if _, err := e.pub.OpenGroup(wire.GroupHeader{TrackID: e.track}); !errors.Is(err, ErrTrackNotFound) {
		t.Fatalf("want ErrTrackNotFound, got %v", err)
	}
	e.sub.Leave("left")
	if rooms, tracks := e.r.Stats(); rooms != 0 || tracks != 0 {
		t.Fatalf("rooms %d tracks %d", rooms, tracks)
	}
	if s := e.m.Snapshot(); s.Subscriptions != 0 || s.Tracks != 0 || s.Rooms != 0 {
		t.Fatalf("gauges %+v", s)
	}
}

func TestPublishLimits(t *testing.T) {
	e := newEnv(t, Options{MaxTracksPerSession: 2, MaxLayers: 2, MaxTrackBitrate: 5_000_000}, false)
	l := func(id uint32, br uint64) *controlpb.Layer { return &controlpb.Layer{Id: id, Bitrate: br} }
	cases := []struct {
		p    *controlpb.Publish
		want error
	}{
		{&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{l(0, 1), l(1, 1), l(2, 1)}}, ErrLimit},
		{&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{l(0, 6_000_000)}}, ErrLimit},
		{&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{l(0, 1), l(0, 1)}}, ErrBadRequest},
		{&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "", Layers: []*controlpb.Layer{l(0, 1)}}, ErrBadRequest},
		{&controlpb.Publish{Codec: "h264", Layers: []*controlpb.Layer{l(0, 1)}}, ErrBadRequest},
		{&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{l(0, 1)}}, nil},
		{&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{l(0, 1)}}, ErrLimit}, // 3rd track
	}
	for i, c := range cases {
		if _, err := e.pub.Publish(c.p); !errors.Is(err, c.want) {
			t.Errorf("case %d: got %v want %v", i, err, c.want)
		}
	}
	if _, err := e.sub.Publish(cases[5].p); !errors.Is(err, ErrNotOwner) {
		t.Errorf("non-publisher: %v", err)
	}
	if err := e.pub.Subscribe(e.track, 0, 0); !errors.Is(err, ErrBadRequest) {
		t.Errorf("self-subscribe: %v", err)
	}
	if err := e.sub.Subscribe(e.track, 9, 0); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad layer: %v", err)
	}
}

func TestScreenDefaultUrgency(t *testing.T) {
	e := newEnv(t, Options{}, false)
	id, err := e.pub.Publish(&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{{Id: 0, Bitrate: 1}}})
	must(t, err)
	must(t, e.sub.Subscribe(id, 0, 0))
	in, err := e.pub.OpenGroup(wire.GroupHeader{TrackID: id, Group: 1})
	must(t, err)
	must(t, in.Frame(frame(true, 10, 's')))
	if w := e.subP.nextWriter(t); w.urgency != 1 {
		t.Fatalf("screen urgency %d, want 1", w.urgency)
	}
}

func TestIngressRateLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	e := newEnv(t, Options{Now: func() time.Time { return now }}, false)
	// Declared 1.8 Mbps × slack 2 = 450 KB/s, burst max(900 KB, 4 MB) = 4 MB.
	in := e.open(0, 1)
	big := frame(true, 1<<20, 'a')
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		err = in.Frame(big)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

func TestRoomFull(t *testing.T) {
	r := New(context.Background(), Options{MaxParticipants: 1})
	_, _, err := r.Join(newPeer(1), Participant{}, "x")
	must(t, err)
	if _, _, err := r.Join(newPeer(2), Participant{}, "x"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("want ErrRoomFull, got %v", err)
	}
}

// A screen share's sound: tied to the publisher's own screen share, one
// layer, one per share, at screen priority, and removed with its share.
func TestScreenAudio(t *testing.T) {
	e := newEnv(t, Options{}, false)
	l := []*controlpb.Layer{{Id: 0, Bitrate: 64_000}}
	audio := func(forID uint32) *controlpb.Publish {
		return &controlpb.Publish{Kind: controlpb.TrackKind_SCREEN_AUDIO, Codec: "opus", Layers: l, ForTrackId: forID}
	}
	screen, err := e.pub.Publish(&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{{Id: 0, Bitrate: 1}}})
	must(t, err)
	for i, p := range []*controlpb.Publish{
		audio(0),       // for nothing
		audio(e.track), // a camera
		audio(999),     // not theirs
		{Kind: controlpb.TrackKind_SCREEN_AUDIO, Codec: "opus", Layers: []*controlpb.Layer{{Id: 0, Bitrate: 1}, {Id: 1, Bitrate: 1}}, ForTrackId: screen}, // two layers
		{Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: l, ForTrackId: screen},                                                                  // only sound has a for_track_id
	} {
		if _, err := e.pub.Publish(p); !errors.Is(err, ErrBadRequest) {
			t.Errorf("case %d: got %v, want bad request", i, err)
		}
	}
	// A viewer that handles sound tracks; e.sub (an older client) doesn't.
	newP := newPeer(3)
	viewer, _, err := e.r.Join(newP, Participant{UserID: "dave", ScreenAudio: true}, "room")
	must(t, err)
	snd, err := e.pub.Publish(audio(screen))
	must(t, err)
	if e.subP.has(func(m *controlpb.ControlMessage) bool { return m.GetTrackAdded().GetTrack().GetTrackId() == snd }) {
		t.Fatal("an older client was told about the sound")
	}
	_, state, err := e.r.Join(newPeer(4), Participant{UserID: "erin"}, "room")
	must(t, err)
	for _, tr := range state.GetTracks() {
		if tr.GetTrackId() == snd {
			t.Fatal("an older client's room state lists the sound")
		}
	}
	if _, err := e.pub.Publish(audio(screen)); !errors.Is(err, ErrBadRequest) {
		t.Errorf("a second sound for one share: %v", err)
	}
	if !newP.has(func(m *controlpb.ControlMessage) bool {
		tr := m.GetTrackAdded().GetTrack()
		return tr.GetTrackId() == snd && tr.GetKind() == controlpb.TrackKind_SCREEN_AUDIO && tr.GetForTrackId() == screen
	}) {
		t.Fatal("no TrackAdded for the sound, tied to its share")
	}
	// Screen priority.
	must(t, viewer.Subscribe(snd, 0, 0))
	in, err := e.pub.OpenGroup(wire.GroupHeader{TrackID: snd, Group: 1})
	must(t, err)
	must(t, in.Frame(frame(true, 80, 'a')))
	if w := newP.nextWriter(t); w.urgency != 1 {
		t.Fatalf("sound urgency %d, want 1", w.urgency)
	}
	// Gone with its share.
	must(t, e.pub.Unpublish(screen))
	if !newP.has(func(m *controlpb.ControlMessage) bool { return m.GetTrackRemoved().GetTrackId() == snd }) {
		t.Fatal("the sound outlived its share")
	}
	if err := e.pub.Unpublish(snd); !errors.Is(err, ErrTrackNotFound) {
		t.Fatalf("unpublish the removed sound: %v", err)
	}
}
