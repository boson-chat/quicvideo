package relay

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
)

// Track is one published video track (camera or screen) with its layers.
type Track struct {
	r      *Relay
	owner  *Member
	id     uint32
	kind   controlpb.TrackKind
	codec  string
	layerP []*controlpb.Layer
	layers map[uint32]*layerState
	subs   map[*Subscription]struct{} // guarded by Relay.mu
	closed atomic.Bool

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
	t.rate = float64(total) / 8 * r.opts.IngressSlack
	// Two seconds at the allowed rate, and at least one max-size frame, so a
	// keyframe burst never trips it.
	t.burst = max(2*t.rate, float64(wire.MaxFrameSize))
	t.tokens = t.burst
	t.lastRefill = r.opts.Now()
	return t
}

func (t *Track) ID() uint32 { return t.id }

func (t *Track) screen() bool { return t.kind == controlpb.TrackKind_SCREEN }

func (t *Track) defaultUrgency() int8 {
	if t.screen() {
		return 1
	}
	return 4
}

func (t *Track) proto() *controlpb.Track {
	return &controlpb.Track{TrackId: t.id, SessionId: t.owner.peer.ID(), Kind: t.kind, Codec: t.codec, Layers: t.layerP}
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

// addSub registers sub on this layer and replays the cached current group
// so the subscriber gets a picture right away. Without a usable cache it
// asks the publisher for a keyframe.
func (l *layerState) addSub(sub *Subscription) {
	l.mu.Lock()
	l.subs[sub] = struct{}{}
	needKF := true
	if l.hasCurrent && l.cacheOK && len(l.cache) > 0 {
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
	} else if g.group != l.current {
		l.mu.Unlock()
		m.FrameDropped(metrics.DropStale, len(f.Payload))
		return ErrStale
	}
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
