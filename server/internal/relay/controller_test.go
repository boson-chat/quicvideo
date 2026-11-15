package relay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
)

func asub(screen bool, urgency int8, id uint32, maxIdx int, rates ...float64) allocSub {
	a := allocSub{screen: screen, urgency: urgency, trackID: id, bitrates: rates, maxIdx: maxIdx}
	for i := range rates {
		a.layers = append(a.layers, uint32(i))
	}
	return a
}

func TestAllocatePriorityOrder(t *testing.T) {
	cam1 := asub(false, 4, 1, 2, 150e3, 500e3, 1200e3)
	cam2 := asub(false, 4, 2, 2, 150e3, 500e3, 1200e3)
	screen := asub(true, 1, 3, 1, 300e3, 2000e3)
	subs := []allocSub{cam1, cam2, screen}
	cases := []struct {
		budget float64
		want   []int
	}{
		{10e6, []int{2, 2, 1}},  // everything fits
		{2.9e6, []int{1, 0, 1}}, // screen top first (2M), then cam1 mid, cam2 low
		{2.6e6, []int{0, 0, 1}}, // screen top, cameras low
		{1e6, []int{1, 0, 0}},   // no room for screen top: screen low, cam1 mid
		{400e3, []int{-1, -1, 0}},
		{100e3, []int{-1, -1, 0}}, // screen always gets its lowest
	}
	for _, c := range cases {
		got := allocate(c.budget, subs)
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("budget %.0f: got %v want %v", c.budget, got, c.want)
				break
			}
		}
		if req := required(subs, got); req > c.budget && c.budget > 300e3 {
			t.Errorf("budget %.0f: allocation needs %.0f", c.budget, req)
		}
	}
	// Max layer is respected.
	capped := []allocSub{asub(false, 4, 1, 0, 150e3, 500e3)}
	if got := allocate(10e6, capped); got[0] != 0 {
		t.Fatalf("max layer ignored: %v", got)
	}
	if c := nextStepCost(subs, []int{0, 0, 1}); c != 350e3 {
		t.Fatalf("next step cost %.0f", c)
	}
}

// ctrlEnv is a relay with a fake clock, driven by Tick.
type ctrlEnv struct {
	*env
	mu  sync.Mutex
	now time.Time
}

func (c *ctrlEnv) clock() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *ctrlEnv) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	c.r.Tick(c.clock())
}

func newCtrlEnv(t *testing.T, initial float64) *ctrlEnv {
	c := &ctrlEnv{now: time.Unix(1000, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts := Options{Metrics: metrics.Nop(), NoLoop: true, InitialBitrate: initial, Now: c.clock}
	r := New(ctx, opts)
	e := &env{t: t, r: r, m: opts.Metrics, pubP: newPeer(1), subP: newPeer(2), cancel: cancel}
	var err error
	e.pub, _, err = r.Join(e.pubP, Participant{UserID: "alice", CanPublish: true}, "room")
	must(t, err)
	e.sub, _, err = r.Join(e.subP, Participant{UserID: "bob"}, "room")
	must(t, err)
	e.track, err = e.pub.Publish(&controlpb.Publish{
		Kind: controlpb.TrackKind_CAMERA, Codec: "h264",
		Layers: []*controlpb.Layer{{Id: 0, Bitrate: 150_000}, {Id: 1, Bitrate: 500_000}, {Id: 2, Bitrate: 1_200_000}},
	})
	must(t, err)
	c.env = e
	return c
}

func (c *ctrlEnv) target() (uint32, bool) {
	target, _, _, paused, _ := c.sub.subs[c.track].state()
	return target, paused
}

func (c *ctrlEnv) lastState() *controlpb.SubscriptionState {
	c.subP.mu.Lock()
	defer c.subP.mu.Unlock()
	for i := len(c.subP.ctrl) - 1; i >= 0; i-- {
		if s := c.subP.ctrl[i].GetSubscriptionState(); s != nil {
			return s
		}
	}
	return nil
}

func TestControllerStartsWithinEstimateAndProbesUp(t *testing.T) {
	c := newCtrlEnv(t, 600_000)
	must(t, c.sub.Subscribe(c.track, 2, 0))
	if l, _ := c.target(); l != 1 {
		t.Fatalf("600k estimate: want layer 1, got %d", l)
	}
	if s := c.lastState(); s == nil || s.GetTargetLayer() != 1 || s.GetMaxLayer() != 2 {
		t.Fatalf("state %v", s)
	}
	// Clean link: probe after StartupHold (2 s) → layer 2.
	for range 9 {
		c.advance(250 * time.Millisecond)
	}
	if l, _ := c.target(); l != 2 {
		t.Fatalf("after startup hold: want layer 2, got %d (est %.0f)", l, c.sub.ctrl.estimate)
	}
}

func TestControllerStepsDownOnCut(t *testing.T) {
	c := newCtrlEnv(t, 5_000_000)
	must(t, c.sub.Subscribe(c.track, 2, 0))
	if l, _ := c.target(); l != 2 {
		t.Fatalf("want 2, got %d", l)
	}
	c.sub.bytesOut.Add(1_200_000 / 8) // ~1.2 Mbps delivered in the last second
	c.advance(time.Second)
	c.sub.cuts.Add(1) // a same-layer group cut
	c.advance(250 * time.Millisecond)
	if l, _ := c.target(); l != 1 {
		t.Fatalf("after a cut: want layer 1, got %d (est %.0f)", l, c.sub.ctrl.estimate)
	}
	est := c.sub.ctrl.estimate
	// More cuts during the cooldown don't count again.
	c.sub.cuts.Add(1)
	c.advance(250 * time.Millisecond)
	if c.sub.ctrl.estimate != est {
		t.Fatal("cooldown ignored")
	}
	// No probe before UpHold (6 s) once congestion has been seen.
	for range 20 {
		c.advance(250 * time.Millisecond)
	}
	if l, _ := c.target(); l != 1 {
		t.Fatalf("probed too early: %d", l)
	}
	for range 8 {
		c.advance(250 * time.Millisecond)
	}
	if l, _ := c.target(); l != 2 {
		t.Fatalf("after UpHold: want 2, got %d", l)
	}
	// The probe fails: back down, and the hold doubles.
	c.sub.cuts.Add(1)
	c.advance(250 * time.Millisecond)
	if l, _ := c.target(); l != 1 || c.sub.ctrl.hold != 12*time.Second {
		t.Fatalf("failed probe: layer %d hold %v", l, c.sub.ctrl.hold)
	}
}

func TestControllerBacklogAndReport(t *testing.T) {
	c := newCtrlEnv(t, 5_000_000)
	c.subP.gated = true // the subscriber's writes block: frames stay queued
	must(t, c.sub.Subscribe(c.track, 2, 0))
	in := c.open(2, 1)
	must(t, in.Frame(frame(true, 10, 'k')))
	w := c.subP.nextWriter(t) // the forwarder is now stuck on it
	must(t, in.Frame(frame(false, 10, 'x')))
	c.advance(200 * time.Millisecond)
	if l, _ := c.target(); l != 2 {
		t.Fatalf("a short backlog (keyframe burst) is not congestion: got %d", l)
	}
	c.advance(250 * time.Millisecond)
	c.advance(250 * time.Millisecond) // third tick in a row over 150 ms
	if l, _ := c.target(); l != 1 {
		t.Fatalf("sustained backlog: want 1, got %d", l)
	}
	for range 10 {
		w.gate <- struct{}{} // the backlog drains
	}
	for c.sub.subs[c.track].backlogAge(c.clock()) > 0 {
		time.Sleep(time.Millisecond)
	}
	// A bad report right after the step down describes the same congestion.
	c.sub.OnReceiverReport(&controlpb.ReceiverReport{TrackId: c.track, Layer: 1, FramesReceived: 30, LateFrames: 10})
	c.advance(1100 * time.Millisecond)
	if l, _ := c.target(); l != 1 {
		t.Fatalf("stale report counted: %d (last tick %+v)", l, c.sub.ctrl.last)
	}
	c.advance(1500 * time.Millisecond)
	c.sub.OnReceiverReport(&controlpb.ReceiverReport{TrackId: c.track, Layer: 1, FramesReceived: 30}) // a good second
	bad := &controlpb.ReceiverReport{TrackId: c.track, Layer: 1, FramesReceived: 30, LateFrames: 10}
	c.sub.OnReceiverReport(bad)
	c.advance(250 * time.Millisecond)
	if l, _ := c.target(); l != 1 {
		t.Fatalf("one bad report (a keyframe burst) is not congestion: %d", l)
	}
	c.sub.OnReceiverReport(bad)
	c.advance(250 * time.Millisecond)
	if l, _ := c.target(); l != 1 {
		t.Fatalf("late frames without any queueing the server can see: %d", l)
	}
	// With the server's send queue also backing up, two bad reports count.
	in1 := c.open(1, 1)
	must(t, in1.Frame(frame(true, 10, 'k')))
	w1 := c.subP.nextWriter(t)
	must(t, in1.Frame(frame(false, 10, 'y')))
	c.advance(100 * time.Millisecond)
	c.sub.OnReceiverReport(bad)
	c.sub.OnReceiverReport(bad)
	c.advance(10 * time.Millisecond)
	if l, _ := c.target(); l != 0 {
		t.Fatalf("corroborated bad reports: want 0, got %d (%+v)", l, c.sub.ctrl.last)
	}
	for range 10 {
		w1.gate <- struct{}{}
	}
}

func TestControllerDownNeverRaisesOthers(t *testing.T) {
	c := newCtrlEnv(t, 2_200_000)
	screen, err := c.pub.Publish(&controlpb.Publish{Kind: controlpb.TrackKind_SCREEN, Codec: "h264",
		Layers: []*controlpb.Layer{{Id: 0, Bitrate: 300_000}, {Id: 1, Bitrate: 2_000_000}}})
	must(t, err)
	must(t, c.sub.Subscribe(screen, 1, 0))
	must(t, c.sub.Subscribe(c.track, 2, 0))
	sl, _, _, _, _ := c.sub.subs[screen].state()
	cl, _ := c.target()
	if sl != 1 || cl != 0 {
		t.Fatalf("2.2M: want screen 1 + camera 0, got %d %d", sl, cl)
	}
	// Congestion: the screen must drop; the camera must not jump up into
	// the budget it freed.
	c.sub.cuts.Add(1)
	c.advance(250 * time.Millisecond)
	sl, _, _, _, _ = c.sub.subs[screen].state()
	cl, _ = c.target()
	if sl != 0 || cl != 0 {
		t.Fatalf("after congestion: screen %d camera %d (est %.0f)", sl, cl, c.sub.ctrl.estimate)
	}
}

func TestControllerPausesWhenNothingFits(t *testing.T) {
	c := newCtrlEnv(t, 100_000)
	must(t, c.sub.Subscribe(c.track, 2, 0))
	if _, paused := c.target(); !paused {
		t.Fatal("camera should be paused below its lowest layer")
	}
	if s := c.lastState(); s == nil || !s.GetPaused() {
		t.Fatalf("state %v", s)
	}
}

func TestLayerActive(t *testing.T) {
	c := newCtrlEnv(t, 1e9)
	active := func() map[uint32]bool {
		c.pubP.mu.Lock()
		defer c.pubP.mu.Unlock()
		out := map[uint32]bool{}
		for _, m := range c.pubP.ctrl {
			if la := m.GetLayerActive(); la != nil {
				out[la.GetLayer()] = la.GetActive()
			}
		}
		return out
	}
	if len(active()) != 0 {
		t.Fatal("no LayerActive before anyone subscribes")
	}
	must(t, c.sub.Subscribe(c.track, 1, 0))
	if a := active(); !a[1] || len(a) != 1 {
		t.Fatalf("after subscribe: %v", a)
	}
	must(t, c.sub.Subscribe(c.track, 0, 0)) // lower max: layer 0 now
	if a := active(); !a[0] || !a[1] {
		t.Fatalf("switching: %v", a)
	}
	// Layer 1 goes inactive only after LayerIdleTimeout.
	c.advance(time.Second)
	if a := active(); !a[1] {
		t.Fatalf("too early: %v", a)
	}
	c.advance(1500 * time.Millisecond)
	c.advance(250 * time.Millisecond)
	if a := active(); a[1] || !a[0] {
		t.Fatalf("after idle: %v", a)
	}
	must(t, c.sub.Unsubscribe(c.track))
	for range 12 {
		c.advance(250 * time.Millisecond)
	}
	if a := active(); a[0] {
		t.Fatalf("after unsubscribe: %v", a)
	}
}

func TestControllerSkipsProbesAboveKnownCapacity(t *testing.T) {
	c := newCtrlEnv(t, 5_000_000)
	c.subP.gated = true
	must(t, c.sub.Subscribe(c.track, 2, 0))
	for range 4 { // fill the rate window
		c.sub.bytesOut.Add(600_000 / 8 / 4)
		c.advance(250 * time.Millisecond)
	}
	in := c.open(2, 1)
	must(t, in.Frame(frame(true, 10, 'k')))
	w := c.subP.nextWriter(t)
	must(t, in.Frame(frame(false, 10, 'x')))
	// Saturated at ~600 kbps delivered: a sustained backlog.
	for range 4 {
		c.sub.bytesOut.Add(600_000 / 8 / 4)
		c.advance(250 * time.Millisecond)
	}
	if l, _ := c.target(); l != 1 {
		t.Fatalf("want 1 after saturation, got %d (%+v)", l, c.sub.ctrl.last)
	}
	for range 10 {
		w.gate <- struct{}{}
	}
	for c.sub.subs[c.track].backlogAge(c.clock()) > 0 {
		time.Sleep(time.Millisecond)
	}
	// Layer 2 needs 1.2 Mbps, twice the capacity seen: no probe for MaxHold.
	for range 4 * 30 {
		c.advance(250 * time.Millisecond)
	}
	if l, _ := c.target(); l != 1 {
		t.Fatalf("probed above known capacity: %d", l)
	}
	for range 4 * 15 {
		c.advance(250 * time.Millisecond)
	}
	if l, _ := c.target(); l != 2 {
		t.Fatalf("after MaxHold the capacity is old news: want 2, got %d", l)
	}
}

func TestUnhealthyIngressLayerIsAvoided(t *testing.T) {
	c := newCtrlEnv(t, 1e9)
	must(t, c.sub.Subscribe(c.track, 2, 0))
	if l, _ := c.target(); l != 2 {
		t.Fatalf("want 2, got %d", l)
	}
	us := func(d time.Duration) uint64 { return uint64(c.clock().Add(-d).UnixMicro()) }
	in0 := c.open(0, 1)
	in2 := c.open(2, 1)
	must(t, in0.Frame(&wire.Frame{Keyframe: true, CaptureUS: us(5 * time.Millisecond), Payload: []byte{1}}))
	must(t, in2.Frame(&wire.Frame{Keyframe: true, CaptureUS: us(5 * time.Millisecond), Payload: []byte{1}}))
	// Layer 2 starts arriving 600 ms late (its publisher's uplink is full);
	// layer 0 stays on time and keeps the baseline honest.
	for range 40 {
		c.mu.Lock()
		c.now = c.now.Add(33 * time.Millisecond)
		c.mu.Unlock()
		must(t, in0.Frame(&wire.Frame{CaptureUS: us(5 * time.Millisecond), Payload: []byte{1}}))
		must(t, in2.Frame(&wire.Frame{CaptureUS: us(600 * time.Millisecond), Payload: []byte{1}}))
		// Let the forwarder keep up in real time, or the fake clock turns its
		// queue into a (genuine-looking) backlog.
		for c.sub.subs[c.track].backlogAge(c.clock()) > 0 {
			time.Sleep(time.Millisecond)
		}
	}
	c.advance(250 * time.Millisecond)
	if l, _ := c.target(); l != 1 {
		t.Fatalf("unhealthy layer 2 should be avoided: got %d", l)
	}
	// After the penalty it's available again (the controller can probe it).
	c.advance(16 * time.Second)
	subs := c.sub.allocSubs()
	if subs[0].maxIdx != 2 {
		t.Fatalf("layer 2 should be back after the penalty: maxIdx %d", subs[0].maxIdx)
	}
}

func TestOneLateFrameDoesNotBenchALayer(t *testing.T) {
	c := newCtrlEnv(t, 1e9)
	must(t, c.sub.Subscribe(c.track, 2, 0))
	us := func(d time.Duration) uint64 { return uint64(c.clock().Add(-d).UnixMicro()) }
	in0 := c.open(0, 1)
	must(t, in0.Frame(&wire.Frame{Keyframe: true, CaptureUS: us(5 * time.Millisecond), Payload: []byte{1}}))
	in2 := c.open(2, 1)
	// One frame 10 s "late" (e.g. a re-sent picture), then on-time ones.
	must(t, in2.Frame(&wire.Frame{Keyframe: true, CaptureUS: us(10 * time.Second), Payload: []byte{1}}))
	for range 5 {
		must(t, in2.Frame(&wire.Frame{CaptureUS: us(5 * time.Millisecond), Payload: []byte{1}}))
	}
	c.advance(250 * time.Millisecond)
	if l, _ := c.target(); l != 2 {
		t.Fatalf("one late frame benched layer 2: target %d", l)
	}
}
