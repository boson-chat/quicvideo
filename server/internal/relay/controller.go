package relay

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
)

// Bandwidth control
//
// Each member (as a subscriber) has an estimate of how many bits/s its
// connection can take. Every ControlInterval the relay updates it from
// what the member's forwarders actually managed to send and from
// congestion signals, then hands the estimate out over the member's
// subscriptions in priority order (screen first, then by urgency) and
// switches layers to match:
//
//   - Down fast. Congestion is any of: a send backlog older than
//     BacklogDown, a group cut on the same layer (superseded or overflow),
//     RTT more than QueueDelayDown above the minimum (the network queue is
//     filling up), or a ReceiverReport with late frames or stalls. Then the
//     estimate drops to 85% of the lower of what was delivered and what the
//     current layers need, and layers step down immediately (the lower
//     layer's cached group is replayed, so there's no wait for a keyframe).
//     A DownCooldown follows, so draining the backlog doesn't count twice.
//   - Up slowly. After UpHold without congestion (StartupHold until the
//     first congestion), the controller probes: it raises the estimate just
//     enough for one more layer step (×1.5 during startup) and switches up
//     at that layer's next keyframe, asking for one if its GOP is long. If
//     congestion follows within ProbeWindow, the hold doubles (up to
//     MaxHold), so a link that can't take the next layer stops being probed
//     often. That's the hysteresis: the estimate only rises when probing and
//     only falls on congestion, so layers don't flap in between.

// NetStats is what a transport can tell about a connection.
type NetStats struct {
	SmoothedRTT, MinRTT      time.Duration
	PacketsSent, PacketsLost uint64
}

// StatsPeer is a Peer that exposes connection stats (QUIC sessions do).
type StatsPeer interface {
	NetStats() NetStats
}

type controller struct {
	estimate float64 // bits/s

	lastTick time.Time
	lastCuts int64
	lastSent uint64
	lastLost uint64
	rate     float64 // delivered bits/s over the last RateWindow
	samples  []rateSample
	// Consecutive ticks with a backlog over BacklogDown. A keyframe is a
	// burst that legitimately queues for a moment; only a backlog that
	// lasts (or is very large) means the link can't keep up.
	backlogTicks int
	// Capacity seen the last time the sender was backlogged (the delivered
	// rate then), to skip probes that clearly can't fit.
	capacity   float64
	capacityAt time.Time

	stableSince   time.Time
	downAt        time.Time
	everCongested bool
	probing       bool
	probeAt       time.Time
	hold          time.Duration
	reportBadAt   time.Time
	// Consecutive bad ReceiverReports per track: one bad second is usually
	// a keyframe burst; two in a row is the link.
	badStreak    map[uint32]int
	reportSevere bool // the last bad report had a long stall

	// Last tick's inputs, for tests and debugging.
	last tickInfo
}

type rateSample struct {
	at    time.Time
	bytes int64
}

// RateWindow is how far back the delivered rate is measured.
const RateWindow = time.Second

// windowRate records the byte counter and returns bits/s over RateWindow
// (0 until the window is filled).
func (c *controller) windowRate(now time.Time, bytes int64) float64 {
	c.samples = append(c.samples, rateSample{now, bytes})
	i := 0
	for i < len(c.samples)-1 && now.Sub(c.samples[i+1].at) >= RateWindow {
		i++
	}
	c.samples = c.samples[i:]
	first := c.samples[0]
	dt := now.Sub(first.at).Seconds()
	if dt < RateWindow.Seconds()*0.75 {
		return 0
	}
	return float64(bytes-first.bytes) * 8 / dt
}

type tickInfo struct {
	rate, estimate, required float64
	backlog                  time.Duration
	cuts                     int64
	qdelay                   time.Duration
	loss                     float64
	report                   bool
	congested                bool
}

func newController(o *Options, now time.Time) *controller {
	return &controller{estimate: o.InitialBitrate, lastTick: now, stableSince: now, hold: o.UpHold, badStreak: map[uint32]int{}}
}

// allocSub is one subscription as the allocator sees it.
type allocSub struct {
	sub      *Subscription
	screen   bool
	urgency  int8
	trackID  uint32
	layers   []uint32  // layer ids, lowest bitrate first
	bitrates []float64 // same order
	maxIdx   int       // highest index the client wants
}

// allocate hands budget out in priority order. It returns, per sub, the
// index into layers to forward, or -1 for paused. Every sub first gets its
// lowest layer while the budget lasts (screen shares always do); then, in
// the same order, each is raised as far as the remaining budget allows.
func allocate(budget float64, subs []allocSub) []int {
	order := make([]int, len(subs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := subs[order[a]], subs[order[b]]
		if x.screen != y.screen {
			return x.screen
		}
		if x.urgency != y.urgency {
			return x.urgency < y.urgency
		}
		return x.trackID < y.trackID
	})
	out := make([]int, len(subs))
	left := budget
	for _, i := range order {
		s := subs[i]
		if s.bitrates[0] <= left || s.screen {
			out[i] = 0
			left -= s.bitrates[0]
		} else {
			out[i] = -1
		}
	}
	for _, i := range order {
		s := subs[i]
		if out[i] < 0 {
			continue
		}
		for j := s.maxIdx; j > out[i]; j-- {
			extra := s.bitrates[j] - s.bitrates[out[i]]
			if extra <= left {
				left -= extra
				out[i] = j
				break
			}
		}
	}
	return out
}

// required is the bits/s an allocation needs.
func required(subs []allocSub, choice []int) float64 {
	var sum float64
	for i, c := range choice {
		if c >= 0 {
			sum += subs[i].bitrates[c]
		}
	}
	return sum
}

// lowestNeed is what every subscription's lowest layer needs together.
func lowestNeed(subs []allocSub) float64 {
	var sum float64
	for _, a := range subs {
		sum += a.bitrates[0]
	}
	return sum
}

// nextStepCost is the extra bits/s for the cheapest single upgrade in
// priority order (0 = everything is at its max).
func nextStepCost(subs []allocSub, choice []int) float64 {
	best := 0.0
	for i, c := range choice {
		s := subs[i]
		var cost float64
		switch {
		case c < 0:
			cost = s.bitrates[0]
		case c < s.maxIdx:
			cost = s.bitrates[c+1] - s.bitrates[c]
		default:
			continue
		}
		if best == 0 || cost < best {
			best = cost
		}
	}
	return best
}

// allocSubs builds the allocator input for a member. Caller holds Relay.mu.
func (m *Member) allocSubs() []allocSub {
	out := make([]allocSub, 0, len(m.subs))
	for _, sub := range m.subs {
		t := sub.track
		sub.mu.Lock()
		maxL, urg := sub.maxLayer, sub.urgency
		sub.mu.Unlock()
		a := allocSub{sub: sub, screen: t.screen(), urgency: urg, trackID: t.id}
		for _, l := range t.byRate {
			a.layers = append(a.layers, l.GetId())
			a.bitrates = append(a.bitrates, float64(l.GetBitrate()))
		}
		a.maxIdx = len(a.layers) - 1
		for j, id := range a.layers {
			if id == maxL {
				a.maxIdx = j
			}
		}
		// Layers the publisher can't deliver on time are off the table.
		now := m.r.opts.Now()
		for a.maxIdx > 0 && !t.layers[a.layers[a.maxIdx]].healthy(now) {
			a.maxIdx--
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].trackID < out[j].trackID })
	return out
}

// reallocate applies the member's current estimate to its subscriptions.
// downOnly (after congestion): nothing moves up, even if a higher-priority
// subscription stepping down freed budget; the next probe hands it out.
// Caller holds Relay.mu.
func (m *Member) reallocate(downOnly ...bool) {
	subs := m.allocSubs()
	if len(subs) == 0 {
		return
	}
	choice := allocate(m.ctrl.estimate, subs)
	if len(downOnly) > 0 && downOnly[0] {
		cur := currentChoice(subs)
		for i := range choice {
			choice[i] = min(choice[i], cur[i])
		}
	}
	for i, c := range choice {
		m.applyChoice(subs[i], c)
	}
}

// applyChoice moves one subscription to the chosen layer index.
func (m *Member) applyChoice(a allocSub, c int) {
	sub := a.sub
	if c < 0 {
		sub.pause()
		return
	}
	layer := a.layers[c]
	target, active, hasActive, paused, _ := sub.state()
	if !paused && target == layer {
		return
	}
	up := hasActive && !paused && rank(a.layers, layer) > rank(a.layers, active)
	sub.mu.Lock()
	urg := sub.urgency
	sub.mu.Unlock()
	if !sub.setTarget(layer, urg) {
		return
	}
	ls := sub.track.layers[layer]
	if up {
		// Switch at the next keyframe; replaying a cached partial group would
		// be a burst right when we're testing for headroom.
		ls.addSub(sub, false)
		if g := ls.gop(); g == 0 || g > m.r.opts.LongGOP {
			ls.requestKeyframe()
		}
		return
	}
	ls.addSub(sub, true)
}

func rank(layers []uint32, id uint32) int { return slices.Index(layers, id) }

// controlTick updates a member's estimate and layers. Caller holds Relay.mu.
func (m *Member) controlTick(now time.Time) {
	c := m.ctrl
	o := &m.r.opts
	dt := now.Sub(c.lastTick).Seconds()
	if dt <= 0 {
		return
	}
	c.lastTick = now
	c.rate = c.windowRate(now, m.bytesOut.Load())
	cuts := m.cuts.Load()
	newCuts := cuts - c.lastCuts
	c.lastCuts = cuts

	var backlog time.Duration
	for _, sub := range m.subs {
		backlog = max(backlog, sub.backlogAge(now))
	}
	var qdelay time.Duration
	var loss float64
	if sp, ok := m.peer.(StatsPeer); ok {
		st := sp.NetStats()
		if st.MinRTT > 0 {
			qdelay = st.SmoothedRTT - st.MinRTT
		}
		if ds := st.PacketsSent - c.lastSent; st.PacketsSent > c.lastSent && ds > 20 {
			loss = float64(st.PacketsLost-min(c.lastLost, st.PacketsLost)) / float64(ds)
		}
		c.lastSent, c.lastLost = st.PacketsSent, st.PacketsLost
	}
	// A report covers the second before it, so one that arrives soon after a
	// step down still describes the congestion that caused it.
	report := !c.reportBadAt.IsZero() && now.Sub(c.reportBadAt) < 2*o.ControlInterval+time.Second &&
		c.reportBadAt.After(c.downAt.Add(o.DownCooldown+time.Second))
	// Reports corroborate: they count when the server also sees some
	// queueing (in the network or in its own send queue), or when the
	// viewer is really starving. Late frames alone can just mean the delay
	// baseline moved (e.g. after a bandwidth change).
	report = report && (qdelay > o.QueueDelayDown/2 || backlog > o.BacklogDown/2 || c.reportSevere)

	subs := m.allocSubs()
	choice := currentChoice(subs)
	for i, c := range choice {
		if c > subs[i].maxIdx {
			// A layer went unhealthy (or the max dropped): step those down now.
			m.reallocate(true)
			subs = m.allocSubs()
			choice = currentChoice(subs)
			break
		}
	}
	need := required(subs, choice)

	if backlog > o.BacklogDown {
		c.backlogTicks++
	} else {
		c.backlogTicks = 0
	}
	sustained := c.backlogTicks >= o.BacklogTicks || backlog > 4*o.BacklogDown
	cooling := now.Sub(c.downAt) < o.DownCooldown
	congested := !cooling && (sustained || newCuts > 0 || qdelay > o.QueueDelayDown || report)
	c.last = tickInfo{rate: c.rate, estimate: c.estimate, required: need, backlog: backlog, cuts: newCuts,
		qdelay: qdelay, loss: loss, report: report, congested: congested}

	if congested && o.Trace != nil {
		o.Trace(m.peer.ID(), fmt.Sprintf("down: est %.0fk need %.0fk rate %.0fk backlog %v (ticks %d) cuts %d qdelay %v loss %.3f report %v",
			c.estimate/1e3, need/1e3, c.rate/1e3, backlog.Round(time.Millisecond), c.backlogTicks, newCuts, qdelay.Round(time.Millisecond), loss, report))
	}
	if congested {
		// At least one step below what the layers need now; lower still if
		// the link delivered clearly less (but no more than halving per
		// step: the measured rate can be low just because little was sent).
		e := c.estimate
		if need > 0 {
			e = min(e, need*0.85)
		}
		// The delivered rate is the link's capacity only while the sender
		// was backlogged; otherwise it's just what the layers needed.
		if sustained && c.rate > 0 {
			c.capacity, c.capacityAt = c.rate, now
			if backlog > 4*o.BacklogDown {
				// Badly behind (e.g. the link just dropped): go straight to
				// what it delivers instead of stepping down second by second.
				e = min(e, c.rate*0.9)
			} else {
				e = min(e, max(c.rate*0.9, need*0.5))
			}
		}
		// Pausing someone's video is a far bigger hit than a slight
		// overshoot: if the link delivered at least what everyone's lowest
		// layer needs, don't go below that. Pausing takes evidence that even
		// the lowest layers don't fit.
		if lowest := lowestNeed(subs); c.rate >= lowest {
			e = max(e, lowest)
		}
		c.estimate = max(e, o.MinBitrate)
		c.downAt, c.stableSince, c.everCongested = now, now, true
		if c.probing {
			c.hold = min(c.hold*2, o.MaxHold)
			c.probing = false
		}
		m.r.m.EstimateDown()
		m.reallocate(true)
		return
	}
	if cooling {
		c.stableSince = now // not yet clean
		return
	}
	if c.probing && now.Sub(c.probeAt) > o.ProbeWindow {
		c.probing = false
		c.hold = o.UpHold // the probe held: back to the normal pace
	}
	hold := c.hold
	if !c.everCongested {
		hold = o.StartupHold
	}
	if c.probing || now.Sub(c.stableSince) < hold || loss > 0.1 {
		return
	}
	step := nextStepCost(subs, choice)
	if step == 0 {
		return // everything at its max
	}
	e := max(c.estimate, need+step*1.1)
	if !c.everCongested {
		e = max(e, c.estimate*1.5)
	}
	if c.capacity > 0 && now.Sub(c.capacityAt) < o.MaxHold && need+step > c.capacity*1.15 {
		// Even the smallest step is well beyond what the link carried when it
		// was last saturated: don't probe until that's old news.
		c.stableSince = now
		return
	}
	c.estimate = min(e, o.MaxEstimate)
	c.probing, c.probeAt, c.stableSince = true, now, now
	if o.Trace != nil {
		o.Trace(m.peer.ID(), fmt.Sprintf("probe: est %.0fk need %.0fk step %.0fk rate %.0fk", c.estimate/1e3, need/1e3, step/1e3, c.rate/1e3))
	}
	m.r.m.EstimateUp()
	m.reallocate()
}

// currentChoice is the allocation the subscriptions are on now (their
// targets).
func currentChoice(subs []allocSub) []int {
	out := make([]int, len(subs))
	for i, a := range subs {
		target, _, _, paused, _ := a.sub.state()
		if paused {
			out[i] = -1
			continue
		}
		out[i] = max(rank(a.layers, target), 0)
	}
	return out
}

// OnReceiverReport takes a client's report on one of its subscriptions.
func (m *Member) OnReceiverReport(r *controlpb.ReceiverReport) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	sub := m.subs[r.GetTrackId()]
	if m.ctrl == nil || sub == nil {
		return
	}
	frames := r.GetFramesReceived()
	// A stall only means congestion if the publisher is actually sending
	// that layer (a camera can just pause).
	stalled := r.GetStallMs() > 300
	if ls := sub.track.layers[r.GetLayer()]; stalled && (ls == nil || !ls.sending(m.r.opts.Now(), 500*time.Millisecond)) {
		stalled = false
	}
	bad := stalled || r.GetDecodeStalls() > 0 || (frames >= 5 && r.GetLateFrames()*4 > frames)
	if !bad {
		m.ctrl.badStreak[r.GetTrackId()] = 0
		return
	}
	m.ctrl.badStreak[r.GetTrackId()]++
	if m.ctrl.badStreak[r.GetTrackId()] >= 2 {
		m.ctrl.reportBadAt = m.r.opts.Now()
		m.ctrl.reportSevere = stalled && r.GetStallMs() > 1000
	}
}

// loop runs the controllers, layer activity and SubscriptionState updates.
func (r *Relay) loop(ctx context.Context) {
	t := time.NewTicker(r.opts.ControlInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(r.opts.Now())
		}
	}
}

// Tick runs one control round (the loop calls it; tests call it directly).
func (r *Relay) Tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rm := range r.rooms {
		for _, mem := range rm.members {
			if len(mem.subs) > 0 {
				mem.controlTick(now)
				mem.sendStates()
			}
		}
	}
	for _, t := range r.tracks {
		t.updateActivity(now)
	}
}

// sendStates sends SubscriptionState for subscriptions whose state
// changed (the estimate alone changing doesn't count). Caller holds
// Relay.mu.
func (m *Member) sendStates() {
	for _, sub := range m.subs {
		target, active, hasActive, paused, maxL := sub.state()
		layer := active
		if !hasActive {
			layer = target
		}
		st := subState{layer: layer, target: target, max: maxL, paused: paused, forwarding: hasActive, valid: true}
		if sub.sent == st {
			continue
		}
		sub.sent = st
		m.peer.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_SubscriptionState{
			SubscriptionState: &controlpb.SubscriptionState{
				TrackId: sub.track.id, Layer: layer, TargetLayer: target, MaxLayer: maxL,
				Paused: paused, EstimateBps: uint64(m.ctrl.estimate), Forwarding: hasActive,
			}}})
	}
}
