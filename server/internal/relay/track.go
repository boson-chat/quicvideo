package relay

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
)

// Track is one published video track (camera or screen) with its layers.
type Track struct {
	r     *Relay
	owner *Member
	id    uint32
	kind  controlpb.TrackKind
	codec string
	// forTrack: a SCREEN_AUDIO track's screen share (0 otherwise).
	forTrack uint32
	layerP   []*controlpb.Layer
	byRate   []*controlpb.Layer // layerP sorted by bitrate, lowest first
	layers   map[uint32]*layerState
	subs     map[*Subscription]struct{} // guarded by Relay.mu
	closed   atomic.Bool

	// Ingress delay baseline over all layers (arrival − capture; the
	// publisher's clock offset cancels): minimum over two rolling windows.
	minCur, minPrev time.Duration
	minSince        time.Time

	// Ingress token bucket over all layers, in bytes.
	bucketMu   sync.Mutex
	tokens     float64
	rate       float64 // bytes/s
	burst      float64
	lastRefill time.Time
}

func newTrack(r *Relay, owner *Member, id uint32, kind controlpb.TrackKind, codec string, layers []*controlpb.Layer, total uint64) *Track {
	t := &Track{r: r, owner: owner, id: id, kind: kind, codec: codec, layers: map[uint32]*layerState{}, subs: map[*Subscription]struct{}{}}
	for _, l := range layers {
		lp := &controlpb.Layer{Id: l.GetId(), Width: l.GetWidth(), Height: l.GetHeight(), Fps: l.GetFps(), Bitrate: l.GetBitrate()}
		t.layerP = append(t.layerP, lp)
		t.layers[lp.Id] = &layerState{t: t, id: lp.Id, subs: map[*Subscription]struct{}{}}
	}
	t.byRate = append([]*controlpb.Layer(nil), t.layerP...)
	sort.SliceStable(t.byRate, func(i, j int) bool { return t.byRate[i].GetBitrate() < t.byRate[j].GetBitrate() })
	t.rate = float64(total) / 8 * r.opts.IngressSlack
	// Two seconds at the allowed rate, and at least one max-size frame, so a
	// keyframe burst never trips it.
	t.burst = max(2*t.rate, float64(wire.MaxFrameSize))
	t.tokens = t.burst
	t.lastRefill = r.opts.Now()
	return t
}

func (t *Track) ID() uint32 { return t.id }

// screen: a screen share or its sound. Both go at screen priority and
// are always allocated (the sound is part of the share).
func (t *Track) screen() bool {
	return t.kind == controlpb.TrackKind_SCREEN || t.kind == controlpb.TrackKind_SCREEN_AUDIO
}

func (t *Track) defaultUrgency() int8 {
	if t.screen() {
		return 1
	}
	return 4
}

func (t *Track) proto() *controlpb.Track {
	return &controlpb.Track{TrackId: t.id, SessionId: t.owner.peer.ID(), Kind: t.kind, Codec: t.codec, Layers: t.layerP, ForTrackId: t.forTrack}
}

// close marks the track gone; in-flight ingests fail with ErrGone.
func (t *Track) close() { t.closed.Store(true) }

// allow takes n bytes from the ingress bucket.
func (t *Track) allow(n int) bool {
	t.bucketMu.Lock()
	defer t.bucketMu.Unlock()
	now := t.r.opts.Now()
	t.tokens = min(t.burst, t.tokens+now.Sub(t.lastRefill).Seconds()*t.rate)
	t.lastRefill = now
	if t.tokens < float64(n) {
		return false
	}
	t.tokens -= float64(n)
	return true
}

// layerState is one simulcast layer: its current group, the cache of that
// group for fast join, and the subscriptions receiving (or switching to) it.
type layerState struct {
	t  *Track
	id uint32

	mu         sync.Mutex
	hasCurrent bool
	current    uint64
	cache      []*wire.Frame
	cacheBytes int
	cacheOK    bool
	subs       map[*Subscription]struct{}
	lastKF     time.Time
	lastStart  time.Time // when the current group started
	lastFrame  time.Time
	// Ingress health: a layer the publisher can't deliver on time (its
	// uplink is too slow) is kept out of allocations for a while.
	lateEWMA       time.Duration
	unhealthyUntil time.Time
	penalty        time.Duration
	healthySince   time.Time
	gopDur         time.Duration // last observed group length

	// Activity (guarded by Relay.mu): whether the publisher was told this
	// layer is wanted, and since when nobody wants it.
	activeSent bool
	idleSince  time.Time
}

// sending: the publisher sent a frame on this layer within d.
func (l *layerState) sending(now time.Time, d time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.lastFrame.IsZero() && now.Sub(l.lastFrame) < d
}

func (l *layerState) gop() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gopDur
}

func (l *layerState) openGroup(g uint64) (*GroupIngest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hasCurrent && g <= l.current {
		l.t.r.m.GroupDropped(metrics.DropStale)
		return nil, ErrStale
	}
	l.t.r.m.IngressGroup()
	return &GroupIngest{l: l, group: g}, nil
}

// addSub registers sub on this layer. With replay, the cached current
// group is replayed so the subscriber gets a picture right away; without a
// usable cache the publisher is asked for a keyframe.
func (l *layerState) addSub(sub *Subscription, replay bool) {
	l.mu.Lock()
	l.subs[sub] = struct{}{}
	needKF := replay
	now := l.t.r.opts.Now()
	// Only replay a live layer's group (an inactive layer's cache is
	// stale), and on a switch only a recent one: replaying a long partial
	// GOP shows old frames and costs bandwidth right when it's short. A
	// fresh keyframe on a lower layer is cheap.
	_, _, switching, _, _ := sub.state()
	live := !l.lastFrame.IsZero() && now.Sub(l.lastFrame) < time.Second
	recent := now.Sub(l.lastStart) <= l.t.r.opts.ReplayMaxAge
	if replay && live && (recent || !switching) && l.hasCurrent && l.cacheOK && len(l.cache) > 0 {
		needKF = false
		for i, f := range l.cache {
			keep, kf := sub.deliver(l.id, l.current, f, i == 0)
			needKF = needKF || kf
			if !keep {
				delete(l.subs, sub)
				break
			}
		}
	}
	l.mu.Unlock()
	if needKF {
		l.requestKeyframe()
	}
}

func (l *layerState) removeSub(sub *Subscription) {
	l.mu.Lock()
	delete(l.subs, sub)
	l.mu.Unlock()
}

// requestKeyframe asks the publisher for a new group on this layer, at
// most once per KeyframeRequestInterval.
func (l *layerState) requestKeyframe() {
	r := l.t.r
	now := r.opts.Now()
	l.mu.Lock()
	if !l.lastKF.IsZero() && now.Sub(l.lastKF) < r.opts.KeyframeRequestInterval {
		l.mu.Unlock()
		return
	}
	l.lastKF = now
	l.mu.Unlock()
	if l.t.closed.Load() {
		return
	}
	r.m.KeyframeRequested()
	l.t.owner.peer.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_KeyframeRequest{
		KeyframeRequest: &controlpb.KeyframeRequest{TrackId: l.t.id, Layer: l.id}}})
}

// GroupIngest receives the frames of one incoming group.
type GroupIngest struct {
	l     *layerState
	group uint64
	n     int
}

func (g *GroupIngest) Group() uint64 { return g.group }

// Frame relays one frame. On error the caller stops reading the stream
// (CancelRead) and calls End(true).
func (g *GroupIngest) Frame(f *wire.Frame) error {
	l := g.l
	t := l.t
	m := t.r.m
	if t.closed.Load() {
		return ErrGone
	}
	m.Ingress(len(f.Payload))
	if !t.allow(len(f.Payload)) {
		m.FrameDropped(metrics.DropRateLimit, len(f.Payload))
		return ErrRateLimited
	}
	first := g.n == 0
	g.n++

	l.mu.Lock()
	if first {
		if l.hasCurrent && g.group <= l.current {
			l.mu.Unlock()
			m.FrameDropped(metrics.DropStale, len(f.Payload))
			return ErrStale
		}
		l.hasCurrent, l.current = true, g.group
		l.cache, l.cacheBytes, l.cacheOK = l.cache[:0], 0, true
		now := t.r.opts.Now()
		if !l.lastStart.IsZero() {
			l.gopDur = now.Sub(l.lastStart)
		}
		l.lastStart = now
	} else if g.group != l.current {
		l.mu.Unlock()
		m.FrameDropped(metrics.DropStale, len(f.Payload))
		return ErrStale
	}
	now := t.r.opts.Now()
	l.lastFrame = now
	l.observeIngress(now, f.CaptureUS)
	if l.cacheOK {
		if l.cacheBytes+len(f.Payload) <= t.r.opts.GroupCacheBytes {
			l.cache = append(l.cache, f)
			l.cacheBytes += len(f.Payload)
		} else {
			l.cache, l.cacheBytes, l.cacheOK = nil, 0, false
		}
	}
	needKF := false
	for sub := range l.subs {
		keep, kf := sub.deliver(l.id, g.group, f, first)
		needKF = needKF || kf
		if !keep {
			delete(l.subs, sub)
		}
	}
	l.mu.Unlock()
	if needKF {
		l.requestKeyframe()
	}
	return nil
}

// End tells subscribers the group is over: aborted = the publisher's
// stream broke or we stopped reading it, so they reset theirs too.
func (g *GroupIngest) End(aborted bool) {
	l := g.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if g.n == 0 || !l.hasCurrent || l.current != g.group {
		return
	}
	if aborted {
		// A partial group is useless to joiners.
		l.cache, l.cacheBytes, l.cacheOK = nil, 0, false
	}
	for sub := range l.subs {
		sub.endGroup(l.id, g.group, aborted)
	}
}

// updateActivity tells the publisher which layers somebody wants: active
// at once, inactive after LayerIdleTimeout with nobody (so a layer switch
// in progress doesn't toggle it). Caller holds Relay.mu.
func (t *Track) updateActivity(now time.Time) {
	if t.closed.Load() {
		return
	}
	want := map[uint32]bool{}
	for sub := range t.subs {
		target, active, hasActive, paused, _ := sub.state()
		if paused {
			continue
		}
		want[target] = true
		if hasActive {
			want[active] = true
		}
	}
	for _, lp := range t.layerP {
		l := t.layers[lp.GetId()]
		switch {
		case want[l.id]:
			l.idleSince = time.Time{}
			if !l.activeSent {
				l.activeSent = true
				t.sendActive(l.id, true)
			}
		case l.activeSent:
			if l.idleSince.IsZero() {
				l.idleSince = now
			} else if now.Sub(l.idleSince) >= t.r.opts.LayerIdleTimeout {
				l.mu.Lock()
				l.cache, l.cacheBytes, l.cacheOK = nil, 0, false
				l.mu.Unlock()
				l.activeSent = false
				l.idleSince = time.Time{}
				t.sendActive(l.id, false)
			}
		}
		// Drop subscriptions that moved away from this layer.
		l.mu.Lock()
		for sub := range l.subs {
			target, active, hasActive, paused, _ := sub.state()
			if paused || (sub.closedNow() || (target != l.id && (!hasActive || active != l.id))) {
				delete(l.subs, sub)
			}
		}
		l.mu.Unlock()
	}
}

func (t *Track) sendActive(layer uint32, active bool) {
	t.owner.peer.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_LayerActive{
		LayerActive: &controlpb.LayerActive{TrackId: t.id, Layer: layer, Active: active}}})
}

// IngressLateDown: a layer whose frames arrive this much later than the
// track's best (smoothed) is unhealthy. Penalties start at 15 s and double
// up to 60 s; a minute of good health resets them.
const (
	IngressLateDown = 250 * time.Millisecond
	minPenalty      = 15 * time.Second
	maxPenalty      = 60 * time.Second
	ingressWindow   = 5 * time.Second
)

// observeIngress updates the layer's lateness. Caller holds l.mu.
func (l *layerState) observeIngress(now time.Time, captureUS uint64) {
	t := l.t
	rel := now.Sub(time.UnixMicro(int64(captureUS)))
	t.bucketMu.Lock() // the baseline is track-wide; the bucket lock is a leaf lock
	switch {
	case t.minSince.IsZero() || now.Sub(t.minSince) > 2*ingressWindow:
		t.minPrev, t.minCur, t.minSince = -1, rel, now
	case now.Sub(t.minSince) > ingressWindow:
		t.minPrev, t.minCur, t.minSince = t.minCur, rel, now
	case rel < t.minCur:
		t.minCur = rel
	}
	base := t.minCur
	if t.minPrev >= 0 && t.minPrev < base {
		base = t.minPrev
	}
	t.bucketMu.Unlock()
	// Cap one frame's say: a single odd frame (a re-sent picture, a clock
	// step) mustn't bench a layer; only sustained lateness does.
	excess := min(max(rel-base, 0), 2*IngressLateDown)
	l.lateEWMA = (l.lateEWMA*9 + excess) / 10
	if !l.healthyLocked(now) {
		return
	}
	if l.lateEWMA > IngressLateDown {
		if l.penalty == 0 || (!l.healthySince.IsZero() && now.Sub(l.healthySince) > time.Minute) {
			l.penalty = minPenalty
		} else {
			l.penalty = min(l.penalty*2, maxPenalty)
		}
		l.unhealthyUntil = now.Add(l.penalty)
		l.lateEWMA, l.healthySince = 0, time.Time{}
		if tr := t.r.opts.Trace; tr != nil {
			go tr(t.owner.peer.ID(), fmt.Sprintf("track %d layer %d: ingress late, unavailable for %v", t.id, l.id, l.penalty))
		}
		return
	}
	if l.healthySince.IsZero() {
		l.healthySince = now
	}
}

func (l *layerState) healthyLocked(now time.Time) bool { return !now.Before(l.unhealthyUntil) }

func (l *layerState) healthy(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.healthyLocked(now)
}
