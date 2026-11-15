package client

import (
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/wire"
)

// LateThreshold: a frame whose relative delay (arrival − capture, minus the
// recent minimum) exceeds this counts as late in ReceiverReports.
const LateThreshold = 150 * time.Millisecond

// recvStats accumulates one subscribed track's reception between reports.
type recvStats struct {
	frames, late, cuts, decodeStalls uint32
	delaySum                         time.Duration
	stall                            time.Duration
	lastArrival                      time.Time
	layer                            uint32
	// Minimum relative delay over two rolling windows (clock offsets cancel).
	minCur, minPrev time.Duration
	minSince        time.Time
}

func newRecvStats() *recvStats { return &recvStats{minCur: -1, minPrev: -1} }

const minWindow = 5 * time.Second

func (r *recvStats) baseline(now time.Time, d time.Duration) time.Duration {
	switch {
	case r.minSince.IsZero() || now.Sub(r.minSince) > 2*minWindow:
		// Nothing recent (start, or after a pause): start over.
		r.minPrev, r.minCur, r.minSince = -1, -1, now
	case now.Sub(r.minSince) > minWindow:
		r.minPrev, r.minCur, r.minSince = r.minCur, -1, now
	}
	if r.minCur < 0 || d < r.minCur {
		r.minCur = d
	}
	b := r.minCur
	if r.minPrev >= 0 && r.minPrev < b {
		b = r.minPrev
	}
	return b
}

// frameInterval is the expected gap between frames of the track's layer.
func (c *Client) frameInterval(trackID, layer uint32) time.Duration {
	if t := c.tracks[trackID]; t != nil {
		for _, l := range t.GetLayers() {
			if l.GetId() == layer && l.GetFps() > 0 {
				return time.Second / time.Duration(l.GetFps())
			}
		}
	}
	return 100 * time.Millisecond
}

func (c *Client) observeFrame(trackID, layer uint32, f *wire.Frame) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.recv[trackID]
	if r == nil {
		return
	}
	rel := now.Sub(time.UnixMicro(int64(f.CaptureUS)))
	excess := rel - r.baseline(now, rel)
	r.frames++
	r.delaySum += excess
	if excess > LateThreshold {
		r.late++
	}
	if !r.lastArrival.IsZero() {
		if gap, limit := now.Sub(r.lastArrival), 3*c.frameInterval(trackID, layer); gap > limit {
			r.stall += gap - limit
		}
	}
	r.lastArrival, r.layer = now, layer
}

func (c *Client) observeCut(trackID uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.recv[trackID]; r != nil {
		r.cuts++
	}
}

// ReportDecodeStall tells the server (in the next ReceiverReport) that the
// app's decoder starved or failed on a track.
func (c *Client) ReportDecodeStall(trackID uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.recv[trackID]; r != nil {
		r.decodeStalls++
	}
}

func (c *Client) reportLoop() {
	t := time.NewTicker(c.opts.ReportInterval)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}
		now := time.Now()
		interval := now.Sub(last)
		last = now
		var reports []*controlpb.ReceiverReport
		c.mu.Lock()
		for id, r := range c.recv {
			st := c.subStates[id]
			if st == nil || st.GetPaused() {
				continue
			}
			// A gap still open at report time is a stall too.
			stall := r.stall
			if !r.lastArrival.IsZero() {
				if gap, limit := now.Sub(r.lastArrival), 3*c.frameInterval(id, r.layer); gap > limit {
					stall += min(gap-limit, interval)
					r.lastArrival = now.Add(-limit) // don't count it twice
				}
			}
			rep := &controlpb.ReceiverReport{
				TrackId: id, Layer: r.layer, FramesReceived: r.frames, GroupsDropped: r.cuts,
				LateFrames: r.late, StallMs: uint32(stall.Milliseconds()), DecodeStalls: r.decodeStalls,
				IntervalMs: uint32(interval.Milliseconds()),
			}
			if r.frames > 0 {
				rep.AvgDelayMs = uint32((r.delaySum / time.Duration(r.frames)).Milliseconds())
			}
			if r.frames > 0 || stall > 0 || r.lastArrival.IsZero() == false {
				reports = append(reports, rep)
			}
			r.frames, r.late, r.cuts, r.decodeStalls, r.delaySum, r.stall = 0, 0, 0, 0, 0, 0
		}
		c.mu.Unlock()
		for _, rep := range reports {
			_ = c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_ReceiverReport{ReceiverReport: rep}})
		}
	}
}
