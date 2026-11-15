// Package relay is the transport-agnostic core of quicvideod: rooms,
// published tracks, subscriptions and the per-subscription forwarders.
//
// Lock order: Relay.mu → layer.mu → Subscription.mu. Nothing that holds a
// layer or subscription lock takes Relay.mu, and Peer.SendControl never
// blocks.
package relay

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
)

var (
	ErrRoomFull      = errors.New("relay: room full")
	ErrAlreadyInRoom = errors.New("relay: already in a room")
	ErrNotInRoom     = errors.New("relay: not in a room")
	ErrBadRequest    = errors.New("relay: bad request")
	ErrLimit         = errors.New("relay: limit exceeded")
	ErrTrackNotFound = errors.New("relay: no such track")
	ErrNotOwner      = errors.New("relay: not the track's owner")
	ErrStale         = errors.New("relay: stale group")
	ErrGone          = errors.New("relay: track gone")
	ErrRateLimited   = errors.New("relay: over declared bitrate")
)

// GroupWriter is an outgoing media stream (a QUIC uni stream).
type GroupWriter interface {
	Write(p []byte) (int, error)
	// Close ends the group normally (FIN).
	Close() error
	// CancelWrite resets the stream with a wire.StreamCode*.
	CancelWrite(code uint64)
}

// Peer is a connected session as the relay sees it.
type Peer interface {
	ID() uint32
	// OpenGroup opens an outgoing media stream with the given QUIC urgency
	// (0..7, lower first). It may block until the peer grants a stream.
	OpenGroup(ctx context.Context, urgency int8) (GroupWriter, error)
	// SendControl queues a control message. It must not block.
	SendControl(m *controlpb.ControlMessage)
}

// Options are the relay limits (see config.RelayConfig).
type Options struct {
	MaxParticipants         int
	MaxTracksPerSession     int
	MaxLayers               int
	MaxTrackBitrate         uint64
	IngressSlack            float64
	GroupCacheBytes         int
	SubQueueBytes           int
	KeyframeRequestInterval time.Duration

	// Bandwidth control (see controller.go). Zero = defaults.
	ControlInterval  time.Duration // 250ms
	InitialBitrate   float64       // 2.5 Mbps: a new subscriber's estimate
	MinBitrate       float64       // 150 kbps
	MaxEstimate      float64       // 100 Mbps
	BacklogDown      time.Duration // 150ms
	BacklogTicks     int           // 3: consecutive ticks over BacklogDown that count as congestion
	QueueDelayDown   time.Duration // 250ms of RTT above the minimum
	DownCooldown     time.Duration // 1s
	UpHold           time.Duration // 6s stable before probing up
	StartupHold      time.Duration // 2s until the first congestion
	MaxHold          time.Duration // 40s after repeated failed probes
	ProbeWindow      time.Duration // 3s for a probe to count as held
	LongGOP          time.Duration // 1s: up-switches ask for a keyframe beyond this
	ReplayMaxAge     time.Duration // 1s: on a switch, replay a cached group only if it started this recently
	LayerIdleTimeout time.Duration // 2s with nobody before LayerActive(false)
	// NoLoop: don't start the control loop (tests drive Tick themselves).
	NoLoop bool
	// Trace, if set, gets one line per controller decision (debugging).
	Trace func(session uint32, msg string)

	Metrics *metrics.Metrics
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
}

func (o *Options) defaults() {
	if o.MaxParticipants <= 0 {
		o.MaxParticipants = 20
	}
	if o.MaxTracksPerSession <= 0 {
		o.MaxTracksPerSession = 4
	}
	if o.MaxLayers <= 0 {
		o.MaxLayers = 4
	}
	if o.MaxTrackBitrate == 0 {
		o.MaxTrackBitrate = 20_000_000
	}
	if o.IngressSlack <= 0 {
		o.IngressSlack = 2
	}
	if o.GroupCacheBytes <= 0 {
		o.GroupCacheBytes = 2 << 20
	}
	if o.SubQueueBytes <= 0 {
		o.SubQueueBytes = 2 << 20
	}
	if o.KeyframeRequestInterval <= 0 {
		o.KeyframeRequestInterval = time.Second
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.ControlInterval, 250*time.Millisecond)
	def(&o.BacklogDown, 150*time.Millisecond)
	if o.BacklogTicks <= 0 {
		o.BacklogTicks = 3
	}
	def(&o.QueueDelayDown, 250*time.Millisecond)
	def(&o.DownCooldown, time.Second)
	def(&o.UpHold, 6*time.Second)
	def(&o.StartupHold, 2*time.Second)
	def(&o.MaxHold, 40*time.Second)
	def(&o.ProbeWindow, 3*time.Second)
	def(&o.LongGOP, time.Second)
	def(&o.ReplayMaxAge, time.Second)
	def(&o.LayerIdleTimeout, 2*time.Second)
	if o.InitialBitrate <= 0 {
		o.InitialBitrate = 2_500_000
	}
	if o.MinBitrate <= 0 {
		o.MinBitrate = 150_000
	}
	if o.MaxEstimate <= 0 {
		o.MaxEstimate = 100_000_000
	}
	if o.Metrics == nil {
		o.Metrics = metrics.Nop()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Relay owns every room and track on this server.
type Relay struct {
	opts Options
	m    *metrics.Metrics
	ctx  context.Context

	mu        sync.Mutex
	rooms     map[string]*room
	tracks    map[uint32]*Track
	nextTrack uint32
}

// New creates a relay. Forwarders stop when ctx is cancelled.
func New(ctx context.Context, opts Options) *Relay {
	opts.defaults()
	r := &Relay{opts: opts, m: opts.Metrics, ctx: ctx, rooms: map[string]*room{}, tracks: map[uint32]*Track{}}
	if !opts.NoLoop {
		go r.loop(ctx)
	}
	return r
}

type room struct {
	name    string
	members map[uint32]*Member
	tracks  map[uint32]*Track
}

func (rm *room) broadcast(msg *controlpb.ControlMessage, except uint32) {
	for id, mem := range rm.members {
		if id != except {
			mem.peer.SendControl(msg)
		}
	}
}

// Participant is the public identity of a member.
type Participant struct {
	UserID      string
	DisplayName string
	CanPublish  bool
}

// Member is a peer that joined a room.
type Member struct {
	r    *Relay
	peer Peer
	info Participant
	room *room // nil after Leave

	tracks map[uint32]*Track
	subs   map[uint32]*Subscription // by track id

	ctrl     *controller  // bandwidth estimate as a subscriber (Relay.mu)
	bytesOut atomic.Int64 // bytes written by this member's forwarders
	cuts     atomic.Int64 // same-layer group cuts (congestion)
}

func (m *Member) Peer() Peer { return m.peer }

// Room is the member's room name ("" after Leave).
func (m *Member) Room() string {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	if m.room == nil {
		return ""
	}
	return m.room.name
}

func (m *Member) participant() *controlpb.Participant {
	return &controlpb.Participant{SessionId: m.peer.ID(), UserId: m.info.UserID, DisplayName: m.info.DisplayName}
}

// Join adds peer to room (created on first join) and returns the room state
// to send it. Everyone else gets ParticipantJoined.
func (r *Relay) Join(peer Peer, info Participant, roomName string) (*Member, *controlpb.RoomState, error) {
	if roomName == "" || len(roomName) > 64 {
		return nil, nil, ErrBadRequest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rm := r.rooms[roomName]
	if rm == nil {
		rm = &room{name: roomName, members: map[uint32]*Member{}, tracks: map[uint32]*Track{}}
		r.rooms[roomName] = rm
		r.m.RoomOpened()
	}
	if len(rm.members) >= r.opts.MaxParticipants {
		if len(rm.members) == 0 {
			delete(r.rooms, roomName)
			r.m.RoomClosed()
		}
		return nil, nil, ErrRoomFull
	}
	mem := &Member{r: r, peer: peer, info: info, room: rm, tracks: map[uint32]*Track{}, subs: map[uint32]*Subscription{},
		ctrl: newController(&r.opts, r.opts.Now())}
	state := &controlpb.RoomState{Room: roomName, YourSessionId: peer.ID()}
	for _, o := range rm.members {
		state.Participants = append(state.Participants, o.participant())
	}
	for _, t := range rm.tracks {
		state.Tracks = append(state.Tracks, t.proto())
	}
	rm.broadcast(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_ParticipantJoined{
		ParticipantJoined: &controlpb.ParticipantJoined{Participant: mem.participant()}}}, 0)
	rm.members[peer.ID()] = mem
	return mem, state, nil
}

// Leave removes the member: its subscriptions end, its tracks are
// unpublished, the others get ParticipantLeft. Safe to call twice.
func (m *Member) Leave(reason string) {
	r := m.r
	r.mu.Lock()
	defer r.mu.Unlock()
	rm := m.room
	if rm == nil {
		return
	}
	for id := range m.subs {
		m.unsubscribeLocked(id)
	}
	for id := range m.tracks {
		m.unpublishLocked(id)
	}
	delete(rm.members, m.peer.ID())
	m.room = nil
	rm.broadcast(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_ParticipantLeft{
		ParticipantLeft: &controlpb.ParticipantLeft{SessionId: m.peer.ID(), Reason: reason}}}, 0)
	if len(rm.members) == 0 {
		delete(r.rooms, rm.name)
		r.m.RoomClosed()
	}
}

// Publish validates and registers a new track and announces it to the room.
func (m *Member) Publish(req *controlpb.Publish) (uint32, error) {
	r := m.r
	if !m.info.CanPublish {
		return 0, ErrNotOwner
	}
	kind := req.GetKind()
	if kind != controlpb.TrackKind_CAMERA && kind != controlpb.TrackKind_SCREEN {
		return 0, ErrBadRequest
	}
	if req.GetCodec() == "" || len(req.GetCodec()) > 32 {
		return 0, ErrBadRequest
	}
	layers := req.GetLayers()
	if len(layers) == 0 {
		return 0, ErrBadRequest
	}
	if len(layers) > r.opts.MaxLayers {
		return 0, ErrLimit
	}
	var total uint64
	seen := map[uint32]bool{}
	for _, l := range layers {
		if seen[l.GetId()] || l.GetId() > 255 {
			return 0, ErrBadRequest
		}
		seen[l.GetId()] = true
		total += l.GetBitrate()
	}
	if total == 0 || total > r.opts.MaxTrackBitrate {
		return 0, ErrLimit
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if m.room == nil {
		return 0, ErrNotInRoom
	}
	if len(m.tracks) >= r.opts.MaxTracksPerSession {
		return 0, ErrLimit
	}
	r.nextTrack++
	t := newTrack(r, m, r.nextTrack, kind, req.GetCodec(), layers, total)
	m.tracks[t.id] = t
	m.room.tracks[t.id] = t
	r.tracks[t.id] = t
	r.m.TrackAdded(t.screen())
	// The publisher hears PublishOK before anyone can subscribe (and send it
	// LayerActive / KeyframeRequest for the new track).
	m.peer.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_PublishOk{
		PublishOk: &controlpb.PublishOK{ClientRef: req.GetClientRef(), TrackId: t.id}}})
	m.room.broadcast(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_TrackAdded{
		TrackAdded: &controlpb.TrackAdded{Track: t.proto()}}}, m.peer.ID())
	return t.id, nil
}

// Unpublish removes one of the member's tracks.
func (m *Member) Unpublish(trackID uint32) error {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	if m.room == nil {
		return ErrNotInRoom
	}
	if _, ok := m.tracks[trackID]; !ok {
		return ErrTrackNotFound
	}
	m.unpublishLocked(trackID)
	return nil
}

func (m *Member) unpublishLocked(trackID uint32) {
	r := m.r
	t := m.tracks[trackID]
	delete(m.tracks, trackID)
	delete(r.tracks, trackID)
	if m.room != nil {
		delete(m.room.tracks, trackID)
	}
	for sub := range t.subs {
		sub.member.removeSubLocked(sub)
	}
	t.close()
	r.m.TrackRemoved(t.screen())
	if m.room != nil {
		m.room.broadcast(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_TrackRemoved{
			TrackRemoved: &controlpb.TrackRemoved{TrackId: trackID}}}, m.peer.ID())
	}
}

// Subscribe starts, or changes, the member's subscription to a track in
// its room. A layer change takes effect at that layer's next group start.
func (m *Member) Subscribe(trackID, layer, priority uint32) error {
	r := m.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if m.room == nil {
		return ErrNotInRoom
	}
	t := m.room.tracks[trackID]
	if t == nil {
		return ErrTrackNotFound
	}
	if t.owner == m {
		return ErrBadRequest
	}
	if t.layers[layer] == nil {
		return ErrBadRequest
	}
	urgency := t.defaultUrgency()
	if priority > 0 {
		urgency = int8(min(priority, 7))
	}
	sub := m.subs[trackID]
	if sub == nil {
		sub = newSubscription(m, t, urgency)
		m.subs[trackID] = sub
		t.subs[sub] = struct{}{}
		r.m.SubscriptionAdded()
		go sub.forward(r.ctx)
	}
	sub.mu.Lock()
	sub.maxLayer, sub.urgency = layer, urgency
	sub.mu.Unlock()
	// The controller picks the layer actually forwarded (≤ max).
	m.reallocate()
	m.sendStates()
	t.updateActivity(r.opts.Now())
	return nil
}

// Unsubscribe ends the member's subscription to a track.
func (m *Member) Unsubscribe(trackID uint32) error {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	if _, ok := m.subs[trackID]; !ok {
		return ErrTrackNotFound
	}
	m.unsubscribeLocked(trackID)
	m.reallocate() // the freed budget goes to the others
	return nil
}

func (m *Member) unsubscribeLocked(trackID uint32) {
	if sub := m.subs[trackID]; sub != nil {
		delete(sub.track.subs, sub)
		m.removeSubLocked(sub)
	}
}

// removeSubLocked drops sub from its member and layers and stops it.
func (m *Member) removeSubLocked(sub *Subscription) {
	delete(m.subs, sub.track.id)
	for _, ls := range sub.track.layers {
		ls.removeSub(sub)
	}
	sub.close()
	m.r.m.SubscriptionRemoved()
}

// OpenGroup is called when the member opens a media stream. It returns
// the ingest for that group, or ErrStale / ErrTrackNotFound / ErrNotOwner.
func (m *Member) OpenGroup(h wire.GroupHeader) (*GroupIngest, error) {
	m.r.mu.Lock()
	t := m.r.tracks[h.TrackID]
	m.r.mu.Unlock()
	if t == nil {
		return nil, ErrTrackNotFound
	}
	if t.owner != m {
		return nil, ErrNotOwner
	}
	ls := t.layers[h.Layer]
	if ls == nil {
		return nil, ErrBadRequest
	}
	return ls.openGroup(h.Group)
}

// Stats is a debug view (tests, /healthz detail later).
func (r *Relay) Stats() (rooms, tracks int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rooms), len(r.tracks)
}
