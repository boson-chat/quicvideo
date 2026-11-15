package relay

import (
	"context"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
)

type groupKey struct {
	layer uint32
	group uint64
}

type item struct {
	key     groupKey
	first   bool
	end     bool // end-of-group marker (no frame)
	aborted bool // with end: reset instead of FIN
	f       *wire.Frame
	enq     time.Time
}

func (it *item) size() int {
	if it.f == nil {
		return 0
	}
	return len(it.f.Payload)
}

// Subscription forwards one track to one member. Frames are queued by the
// publisher-side goroutines (deliver) and written by a single forwarder
// goroutine, one outgoing stream per group.
type Subscription struct {
	member *Member
	track  *Track
	m      *metrics.Metrics
	cap    int

	mu         sync.Mutex
	urgency    int8
	maxLayer   uint32 // from Subscribe: the best layer the client wants
	paused     bool   // controller: not enough bandwidth for the lowest layer
	target     uint32 // controller's choice
	active     uint32
	hasActive  bool
	curKey     groupKey // group being queued; frames of other groups are dropped
	hasCur     bool
	skipping   bool // current group dropped (overflow): ignore its remaining frames
	queue      []item
	queueBytes int
	writer     GroupWriter // stream of writerKey, owned by the forwarder
	writerKey  groupKey
	closed     bool
	notify     chan struct{}
	done       chan struct{}

	sent subState // last SubscriptionState sent (guarded by Relay.mu)
}

type subState struct {
	layer, target, max uint32
	paused, forwarding bool
	valid              bool
}

func newSubscription(m *Member, t *Track, urgency int8) *Subscription {
	return &Subscription{
		member: m, track: t, m: m.r.m, cap: m.r.opts.SubQueueBytes, urgency: urgency,
		paused: true, // until the controller assigns a layer
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

// setTarget sets the wanted layer and urgency. It reports whether the
// subscription must be (re)registered on the target layer.
func (s *Subscription) setTarget(layer uint32, urgency int8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urgency = urgency
	changed := s.target != layer || !s.hasActive || s.paused
	s.target, s.paused = layer, false
	return changed
}

// pause stops forwarding (not enough bandwidth). The open group is reset.
func (s *Subscription) pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused {
		return
	}
	s.paused, s.hasActive, s.hasCur = true, false, false
	for _, it := range s.queue {
		if it.f != nil {
			s.m.FrameDropped(metrics.DropPaused, it.size())
		}
	}
	clear(s.queue)
	s.queue, s.queueBytes = s.queue[:0], 0
	s.cancelWriter()
}

// state reads what the subscription is doing (for SubscriptionState and
// the controller).
func (s *Subscription) state() (target, active uint32, hasActive, paused bool, max uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target, s.active, s.hasActive, s.paused, s.maxLayer
}

func (s *Subscription) closedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// backlogAge is how long the oldest queued frame has waited.
func (s *Subscription) backlogAge(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.queue {
		if it.f != nil {
			return now.Sub(it.enq)
		}
	}
	return 0
}

// Layer is the layer currently being forwarded (ok=false: none yet).
func (s *Subscription) Layer() (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.hasActive
}

func (s *Subscription) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// deliver queues a frame from layer. Called with that layer's lock held.
// keep=false tells the layer to drop this subscription from its set (it
// was switched away from). needKF asks for a keyframe on that layer.
func (s *Subscription) deliver(layer uint32, group uint64, f *wire.Frame, first bool) (keep, needKF bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.paused {
		return false, false
	}
	if layer != s.target && (!s.hasActive || layer != s.active) {
		return false, false
	}
	key := groupKey{layer, group}
	if layer != s.active || !s.hasActive {
		// Only the target layer gets here: switch at its group start.
		if !first {
			return true, true
		}
		if s.hasActive {
			s.m.LayerSwitched()
		}
		s.active, s.hasActive = layer, true
	}
	// From here layer == active (possibly still the old layer while the
	// target's next keyframe hasn't arrived).
	keep = true

	if first {
		if s.hasCur && s.curKey != key {
			// Same layer: the subscriber fell behind (a congestion signal).
			// Different layer: an intended switch, not congestion.
			s.supersede(s.curKey.layer == layer)
		}
		s.curKey, s.hasCur, s.skipping = key, true, false
	} else if !s.hasCur || s.curKey != key {
		s.m.FrameDropped(metrics.DropNoSubscriber, len(f.Payload))
		return keep, true
	} else if s.skipping {
		s.m.FrameDropped(metrics.DropOverflow, len(f.Payload))
		return keep, false
	}

	if s.queueBytes+len(f.Payload) > s.cap {
		s.dropCurrent(metrics.DropOverflow)
		s.m.FrameDropped(metrics.DropOverflow, len(f.Payload))
		return keep, true
	}
	s.queue = append(s.queue, item{key: key, first: first, f: f, enq: s.member.r.opts.Now()})
	s.queueBytes += len(f.Payload)
	s.wake()
	return keep, false
}

// endGroup queues the end of a group. Called with the layer lock held.
func (s *Subscription) endGroup(layer uint32, group uint64, aborted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := groupKey{layer, group}
	if s.closed || !s.hasCur || s.curKey != key || s.skipping {
		return
	}
	s.queue = append(s.queue, item{key: key, end: true, aborted: aborted})
	s.wake()
}

// supersede drops queued frames of older groups: a new group started while
// the subscriber was still behind. The stream they belong to is reset so
// QUIC stops (re)transmitting it. Caller holds s.mu.
func (s *Subscription) supersede(congestion bool) {
	// Everything queued belongs to older groups. End markers go too: the
	// reset below, or the forwarder opening the next group, ends the old
	// stream.
	dropped := false
	var oldest time.Time
	for _, it := range s.queue {
		if it.f != nil {
			s.m.FrameDropped(metrics.DropSuperseded, it.size())
			if !dropped || it.enq.Before(oldest) {
				oldest = it.enq
			}
			dropped = true
		}
	}
	clear(s.queue)
	s.queue, s.queueBytes = s.queue[:0], 0
	if dropped {
		s.m.GroupDropped(metrics.DropSuperseded)
		s.cancelWriter()
		// Only a backlog that had waited means the subscriber fell behind.
		// Two keyframes back to back (a new layer's first frame and a
		// keyframe request, say) supersede a frame that was queued a moment
		// ago: that's not congestion.
		if congestion && s.member.r.opts.Now().Sub(oldest) >= s.member.r.opts.BacklogDown/3 {
			s.member.cuts.Add(1)
		}
	}
}

// dropCurrent discards the queued part of the current group and ignores
// the rest of it. Caller holds s.mu.
func (s *Subscription) dropCurrent(reason metrics.DropReason) {
	kept := s.queue[:0]
	for _, it := range s.queue {
		if it.key == s.curKey {
			s.queueBytes -= it.size()
			if it.f != nil {
				s.m.FrameDropped(reason, it.size())
			}
			continue
		}
		kept = append(kept, it)
	}
	s.queue = kept
	s.skipping = true
	s.m.GroupDropped(reason)
	s.member.cuts.Add(1)
	if s.writer != nil && s.writerKey == s.curKey {
		s.cancelWriter()
	}
}

func (s *Subscription) cancelWriter() {
	if s.writer != nil {
		s.writer.CancelWrite(wire.StreamCodeDropped)
		s.writer = nil
	}
}

// close stops the forwarder; its open stream is reset.
func (s *Subscription) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.queue, s.queueBytes = nil, 0
		s.wake()
	}
	s.mu.Unlock()
}

// Done is closed when the forwarder has exited.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// forward writes queued frames until the subscription is closed or ctx ends.
func (s *Subscription) forward(ctx context.Context) {
	defer close(s.done)
	peer := s.member.peer
	var broken groupKey
	var hasBroken bool
	hdr := make([]byte, 0, 32)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.mu.Unlock()
			select {
			case <-s.notify:
			case <-ctx.Done():
				s.close()
			}
			s.mu.Lock()
		}
		if s.closed {
			w := s.writer
			s.writer = nil
			s.mu.Unlock()
			if w != nil {
				w.CancelWrite(wire.StreamCodeGone)
			}
			return
		}
		it := s.queue[0]
		s.queue[0] = item{}
		s.queue = s.queue[1:]
		s.queueBytes -= it.size()
		w, wk, urgency := s.writer, s.writerKey, s.urgency
		s.mu.Unlock()

		if hasBroken && it.key == broken {
			if it.f != nil {
				s.m.FrameDropped(metrics.DropWriteError, it.size())
			}
			continue
		}
		if it.end {
			if w != nil && wk == it.key {
				if it.aborted {
					w.CancelWrite(wire.StreamCodeDropped)
				} else {
					_ = w.Close()
				}
				s.mu.Lock()
				if s.writer == w {
					s.writer = nil
				}
				s.mu.Unlock()
			}
			continue
		}
		if w == nil || wk != it.key {
			if w != nil {
				_ = w.Close() // previous group, complete as far as we know
			}
			if !it.first {
				// Its stream was reset (superseded/overflow): wait for the next group.
				s.m.FrameDropped(metrics.DropSuperseded, it.size())
				continue
			}
			nw, err := peer.OpenGroup(ctx, urgency)
			if err != nil {
				if ctx.Err() != nil {
					s.close()
					continue
				}
				broken, hasBroken = it.key, true
				s.m.FrameDropped(metrics.DropWriteError, it.size())
				continue
			}
			// Install the writer before the first (possibly blocking) write,
			// so deliver can reset it if this group is superseded meanwhile.
			s.mu.Lock()
			if s.closed || s.curKey != it.key || s.skipping {
				s.mu.Unlock()
				nw.CancelWrite(wire.StreamCodeDropped)
				broken, hasBroken = it.key, true
				s.m.FrameDropped(metrics.DropSuperseded, it.size())
				continue
			}
			s.writer, s.writerKey = nw, it.key
			s.mu.Unlock()
			hdr = wire.AppendGroupHeader(hdr[:0], wire.GroupHeader{TrackID: s.track.id, Layer: it.key.layer, Group: it.key.group})
			if _, err := nw.Write(hdr); err != nil {
				s.mu.Lock()
				if s.writer == nw {
					s.writer = nil
				}
				s.mu.Unlock()
				nw.CancelWrite(wire.StreamCodeGone)
				broken, hasBroken = it.key, true
				s.m.FrameDropped(metrics.DropWriteError, it.size())
				continue
			}
			s.m.EgressGroup()
			w = nw
		}
		hdr = wire.AppendFrameHeader(hdr[:0], it.f)
		_, err := w.Write(hdr)
		if err == nil {
			_, err = w.Write(it.f.Payload)
		}
		if err != nil {
			s.mu.Lock()
			if s.writer == w {
				s.writer = nil
			}
			s.mu.Unlock()
			w.CancelWrite(wire.StreamCodeDropped)
			broken, hasBroken = it.key, true
			s.m.FrameDropped(metrics.DropWriteError, it.size())
			continue
		}
		s.m.Egress(it.size())
		s.member.bytesOut.Add(int64(len(hdr) + it.size()))
	}
}
