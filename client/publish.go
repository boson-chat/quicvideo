package client

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
)

// PublishOptions tune a published track's send side.
type PublishOptions struct {
	// Per-layer send queue. When the oldest queued frame is older than
	// MaxQueueDelay (default 300ms) or the queue would exceed MaxQueueBytes
	// (default 2 MB), frames are dropped until the next keyframe and the
	// keyframe-needed callback fires.
	MaxQueueDelay time.Duration
	MaxQueueBytes int
	// SendInactiveLayers sends every layer even when the server says nobody
	// watches it (by default those frames are refused with ErrLayerInactive,
	// so the app can skip encoding them).
	SendInactiveLayers bool
	// OnLayerActive is called when the server reports a layer gained its
	// first viewer or lost its last. Start an active layer with a keyframe.
	OnLayerActive func(layer uint32, active bool)
	// OnKeyframeNeeded is called when a layer needs a keyframe: the server
	// asked, the layer just became active, or the send queue dropped frames.
	// The same layer ids also arrive on KeyframeRequests().
	OnKeyframeNeeded func(layer uint32)
	// ForTrack: a SCREEN_AUDIO track's screen share (one of ours).
	ForTrack uint32
}

// LayerStats is a snapshot of one layer's send side.
type LayerStats struct {
	Layer           uint32
	Active          bool
	FramesSent      uint64
	BytesSent       uint64
	FramesDropped   uint64 // backpressure or write errors
	FramesInactive  uint64 // refused because nobody watches the layer
	Groups          uint64
	KeyframesNeeded uint64
	QueueFrames     int
	QueueBytes      int
	QueueDelay      time.Duration // age of the oldest queued frame
}

// LocalTrack is a published track. WriteFrame never blocks on the network:
// each layer has a bounded queue drained by its own goroutine.
type LocalTrack struct {
	c       *Client
	id      uint32
	urgency int8
	opts    PublishOptions
	layers  map[uint32]*localLayer
	kfReq   chan uint32
	done    chan struct{}
	stopped sync.Once
}

type qframe struct {
	f   wire.Frame
	enq time.Time
	end bool // close the current group after what's queued
}

type localLayer struct {
	t       *LocalTrack
	id      uint32
	urgency int8

	mu       sync.Mutex
	q        []qframe
	qBytes   int
	inGroup  bool // queue side: a keyframe has been accepted for the current group
	dropping bool // dropping until the next keyframe
	active   bool
	notify   chan struct{}
	stats    LayerStats
}

func newLocalTrack(c *Client, id uint32, kind controlpb.TrackKind, layers []*controlpb.Layer, o PublishOptions) *LocalTrack {
	if o.MaxQueueDelay <= 0 {
		o.MaxQueueDelay = 300 * time.Millisecond
	}
	if o.MaxQueueBytes <= 0 {
		o.MaxQueueBytes = 2 << 20
	}
	urgency := int8(4)
	if kind == controlpb.TrackKind_SCREEN || kind == controlpb.TrackKind_SCREEN_AUDIO {
		urgency = 1
	}
	t := &LocalTrack{c: c, id: id, urgency: urgency, opts: o, layers: map[uint32]*localLayer{},
		kfReq: make(chan uint32, 16), done: make(chan struct{})}
	// On a congested uplink, lower layers go first: everyone can use them.
	byRate := append([]*controlpb.Layer(nil), layers...)
	sort.SliceStable(byRate, func(i, j int) bool { return byRate[i].GetBitrate() < byRate[j].GetBitrate() })
	for rank, l := range byRate {
		ll := &localLayer{t: t, id: l.GetId(), urgency: min(urgency+int8(rank), 7), notify: make(chan struct{}, 1)}
		ll.stats.Layer = ll.id
		t.layers[ll.id] = ll
		go ll.run()
	}
	return t
}

func (t *LocalTrack) ID() uint32 { return t.id }

// KeyframeRequests delivers layer ids that need a keyframe (see
// PublishOptions.OnKeyframeNeeded).
func (t *LocalTrack) KeyframeRequests() <-chan uint32 { return t.kfReq }

// LayerActive reports whether anyone watches a layer.
func (t *LocalTrack) LayerActive(layer uint32) bool {
	l := t.layers[layer]
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// Stats returns per-layer send statistics.
func (t *LocalTrack) Stats() []LayerStats {
	out := make([]LayerStats, 0, len(t.layers))
	now := time.Now()
	for _, l := range t.layers {
		l.mu.Lock()
		s := l.stats
		s.Active = l.active
		s.QueueFrames, s.QueueBytes = len(l.q), l.qBytes
		for _, q := range l.q {
			if !q.end {
				s.QueueDelay = now.Sub(q.enq)
				break
			}
		}
		l.mu.Unlock()
		out = append(out, s)
	}
	return out
}

func (t *LocalTrack) keyframeNeeded(layer uint32) {
	if l := t.layers[layer]; l != nil {
		l.mu.Lock()
		l.stats.KeyframesNeeded++
		l.mu.Unlock()
	}
	select {
	case t.kfReq <- layer:
	default:
	}
	if t.opts.OnKeyframeNeeded != nil {
		t.opts.OnKeyframeNeeded(layer)
	}
}

func (t *LocalTrack) setActive(layer uint32, active bool) {
	l := t.layers[layer]
	if l == nil {
		return
	}
	l.mu.Lock()
	changed := l.active != active
	l.active = active
	if !active && !t.opts.SendInactiveLayers {
		l.dropQueueLocked()
		l.inGroup = false
		l.q = append(l.q, qframe{end: true})
		l.wake()
	}
	l.mu.Unlock()
	if !changed {
		return
	}
	if t.opts.OnLayerActive != nil {
		t.opts.OnLayerActive(layer, active)
	}
	if active {
		t.keyframeNeeded(layer)
	}
}

// WriteFrame queues one encoded frame on a layer and returns at once. A
// keyframe starts a new group (stream); other frames continue it. The
// payload is copied. Errors: ErrNeedKeyframe (no group open),
// ErrLayerInactive (nobody watches it), ErrBackpressure (the queue is
// backed up: frames are dropped until the next keyframe, which has been
// asked for). captureUS = 0 uses the current time.
func (t *LocalTrack) WriteFrame(layer uint32, keyframe bool, captureUS uint64, payload []byte) error {
	l := t.layers[layer]
	if l == nil {
		return ErrNoLayer
	}
	now := time.Now()
	if captureUS == 0 {
		captureUS = uint64(now.UnixMicro())
	}
	l.mu.Lock()
	if !l.active && !t.opts.SendInactiveLayers {
		l.stats.FramesInactive++
		l.mu.Unlock()
		return ErrLayerInactive
	}
	backlogged := l.qBytes+len(payload) > t.opts.MaxQueueBytes || l.oldestLocked(now) > t.opts.MaxQueueDelay
	switch {
	case keyframe:
		if backlogged {
			// Whatever is still queued is stale now: drop it and start over
			// with this keyframe.
			l.dropQueueLocked()
		}
		l.dropping, l.inGroup = false, true
	case l.dropping:
		l.stats.FramesDropped++
		l.mu.Unlock()
		return ErrBackpressure
	case !l.inGroup:
		l.mu.Unlock()
		return ErrNeedKeyframe
	case backlogged:
		l.stats.FramesDropped++
		l.dropping, l.inGroup = true, false
		// What's queued is a valid prefix of the group: send it, then end
		// the group.
		l.q = append(l.q, qframe{end: true})
		l.wake()
		l.mu.Unlock()
		t.keyframeNeeded(layer)
		return ErrBackpressure
	}
	l.q = append(l.q, qframe{f: wire.Frame{Keyframe: keyframe, CaptureUS: captureUS, Payload: append([]byte(nil), payload...)}, enq: now})
	l.qBytes += len(payload)
	l.wake()
	l.mu.Unlock()
	return nil
}

func (l *localLayer) wake() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *localLayer) oldestLocked(now time.Time) time.Duration {
	for _, q := range l.q {
		if !q.end {
			return now.Sub(q.enq)
		}
	}
	return 0
}

// dropQueueLocked discards queued frames (keeping end markers so the open
// group still gets closed).
func (l *localLayer) dropQueueLocked() {
	kept := l.q[:0]
	for _, q := range l.q {
		if q.end {
			kept = append(kept, q)
			continue
		}
		l.stats.FramesDropped++
	}
	if len(kept) == 0 && len(l.q) > 0 {
		kept = append(kept, qframe{end: true})
	}
	l.q, l.qBytes = kept, 0
}

// run drains the layer's queue onto QUIC streams.
func (l *localLayer) run() {
	t := l.t
	conn := t.c.conn
	var st *quic.SendStream
	var group uint64
	hdr := make([]byte, 0, 32)
	closeStream := func() {
		if st != nil {
			_ = st.Close()
			st = nil
		}
	}
	defer func() {
		if st != nil {
			st.CancelWrite(wire.StreamCodeGone)
		}
	}()
	for {
		l.mu.Lock()
		for len(l.q) == 0 {
			l.mu.Unlock()
			select {
			case <-l.notify:
			case <-t.done:
				return
			case <-conn.Context().Done():
				return
			}
			l.mu.Lock()
		}
		q := l.q[0]
		l.q[0] = qframe{}
		l.q = l.q[1:]
		l.qBytes -= len(q.f.Payload)
		l.mu.Unlock()

		if q.end {
			closeStream()
			continue
		}
		if q.f.Keyframe {
			closeStream()
			s, err := conn.OpenUniStream()
			if err != nil {
				ctx, cancel := context.WithTimeout(conn.Context(), 2*time.Second)
				s, err = conn.OpenUniStreamSync(ctx)
				cancel()
			}
			if err != nil {
				l.writeFailed()
				continue
			}
			s.SetPriority(l.urgency, false)
			group++
			st = s
			hdr = wire.AppendGroupHeader(hdr[:0], wire.GroupHeader{TrackID: t.id, Layer: l.id, Group: group})
			if _, err := st.Write(hdr); err != nil {
				st.CancelWrite(wire.StreamCodeGone)
				st = nil
				l.writeFailed()
				continue
			}
			l.mu.Lock()
			l.stats.Groups++
			l.mu.Unlock()
		}
		if st == nil {
			continue // the group's stream failed: skip to the next keyframe
		}
		hdr = wire.AppendFrameHeader(hdr[:0], &q.f)
		_, err := st.Write(hdr)
		if err == nil {
			_, err = st.Write(q.f.Payload)
		}
		if err != nil {
			// Typically the server stopped reading (stale/dropped group).
			st.CancelWrite(wire.StreamCodeGone)
			st = nil
			l.writeFailed()
			continue
		}
		l.mu.Lock()
		l.stats.FramesSent++
		l.stats.BytesSent += uint64(len(q.f.Payload))
		l.mu.Unlock()
	}
}

// writeFailed drops the rest of the group and asks for a keyframe.
func (l *localLayer) writeFailed() {
	l.mu.Lock()
	l.stats.FramesDropped++
	if !l.dropping {
		// Drop queued frames of the broken group (up to the next keyframe).
		i := 0
		for i < len(l.q) && !l.q[i].f.Keyframe {
			l.qBytes -= len(l.q[i].f.Payload)
			i++
		}
		l.q = l.q[i:]
		if len(l.q) == 0 {
			l.dropping, l.inGroup = true, false
		}
	}
	l.mu.Unlock()
	l.t.keyframeNeeded(l.id)
}

// EndGroup closes the current group on a layer without starting a new one
// (e.g. the encoder is paused).
func (t *LocalTrack) EndGroup(layer uint32) {
	if l := t.layers[layer]; l != nil {
		l.mu.Lock()
		l.inGroup = false
		l.q = append(l.q, qframe{end: true})
		l.wake()
		l.mu.Unlock()
	}
}

func (t *LocalTrack) stop() {
	t.stopped.Do(func() { close(t.done) })
}

// Unpublish stops the track.
func (t *LocalTrack) Unpublish() error {
	t.stop()
	t.c.mu.Lock()
	delete(t.c.local, t.id)
	t.c.mu.Unlock()
	return t.c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Unpublish{Unpublish: &controlpb.Unpublish{TrackId: t.id}}})
}
