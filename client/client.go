// Package client is the Go SDK for quicvideod: connect, join a room,
// publish simulcast tracks and subscribe to other people's tracks.
//
// The SDK moves encoded frames; capturing and encoding (and decoding on the
// receive side) are the application's job.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
)

var (
	ErrClosed        = errors.New("client: closed")
	ErrNeedKeyframe  = errors.New("client: a group must start with a keyframe")
	ErrNoLayer       = errors.New("client: no such layer")
	ErrLayerInactive = errors.New("client: nobody is watching this layer")
	ErrBackpressure  = errors.New("client: send queue full, dropping until the next keyframe")
)

// ServerError is an Error message from the server.
type ServerError struct{ Msg *controlpb.Error }

func (e *ServerError) Error() string {
	return fmt.Sprintf("quicvideo: %s: %s", e.Msg.GetCode(), e.Msg.GetMessage())
}

// Code is the server's error code.
func (e *ServerError) Code() controlpb.Error_Code { return e.Msg.GetCode() }

// RemoteFrame is a frame received on a subscription.
type RemoteFrame struct {
	TrackID uint32
	Layer   uint32
	Group   uint64
	// First frame of its group (always a keyframe): a decoder can start here.
	GroupStart bool
	wire.Frame
}

// Options configure Dial.
type Options struct {
	Token         string
	ClientVersion string
	// Capabilities go in Hello: CapScreenAudio asks to hear about screen
	// shares' sound tracks (servers hide them from clients that don't).
	Capabilities []string
	// TLS config; nil = system roots. ServerName defaults to the host.
	TLS *tls.Config
	// OnFrame is called for each received frame, from one goroutine per
	// group stream (frames of one group arrive in order). It must not block
	// for long.
	OnFrame func(RemoteFrame)
	// OnGroupEnd is called when a received group ends: complete=false means
	// the server reset it (dropped/superseded); wait for the next GroupStart.
	OnGroupEnd func(trackID, layer uint32, group uint64, complete bool)
	// Events buffer size (default 256). Events must be consumed.
	EventBuffer int

	// TokenSource, if set, is called for a fresh token shortly before the
	// current one expires (and when the server says it has); the SDK then
	// sends Refresh.
	TokenSource func(ctx context.Context) (string, error)
	// NoReceiverReports turns off the once-a-second ReceiverReport per
	// subscribed track (the server's bandwidth control uses them).
	NoReceiverReports bool
	// ReportInterval for ReceiverReports (default 1s).
	ReportInterval time.Duration
}

// Client is one connection to quicvideod.
type Client struct {
	conn    *quic.Conn
	ctrl    *quic.Stream
	opts    Options
	welcome *controlpb.Welcome
	events  chan *controlpb.ControlMessage

	writeMu sync.Mutex

	mu          sync.Mutex
	joinWait    chan *controlpb.ControlMessage
	pubWait     map[string]chan *controlpb.ControlMessage
	refreshWait chan *controlpb.ControlMessage
	local       map[uint32]*LocalTrack
	tracks      map[uint32]*controlpb.Track             // remote tracks in the room
	subStates   map[uint32]*controlpb.SubscriptionState // latest per subscribed track
	recv        map[uint32]*recvStats                   // per subscribed track
	expiresAt   time.Time
	refreshKick chan struct{}
	// LayerActive for a track whose PublishOK hasn't been processed yet.
	earlyActive map[uint32]map[uint32]bool
	nextRef     atomic.Uint64
	closed      chan struct{}
	closeErr    error
}

// QUICConfig is the client's quic-go config.
func QUICConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 30 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		MaxIncomingStreams:             -1,
		MaxIncomingUniStreams:          1000,
		InitialStreamReceiveWindow:     1 << 20,
		MaxStreamReceiveWindow:         8 << 20,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
	}
}

// Dial connects, authenticates and waits for Welcome.
func Dial(ctx context.Context, addr string, opts Options) (*Client, error) {
	tlsConf := &tls.Config{}
	if opts.TLS != nil {
		tlsConf = opts.TLS.Clone()
	}
	tlsConf.NextProtos = []string{wire.ALPN}
	tlsConf.MinVersion = tls.VersionTLS13
	conn, err := quic.DialAddr(ctx, addr, tlsConf, QUICConfig())
	if err != nil {
		return nil, err
	}
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	ctrl.SetPriority(0, false)
	if opts.EventBuffer <= 0 {
		opts.EventBuffer = 256
	}
	c := &Client{
		conn: conn, ctrl: ctrl, opts: opts,
		events:      make(chan *controlpb.ControlMessage, opts.EventBuffer),
		pubWait:     map[string]chan *controlpb.ControlMessage{},
		local:       map[uint32]*LocalTrack{},
		tracks:      map[uint32]*controlpb.Track{},
		subStates:   map[uint32]*controlpb.SubscriptionState{},
		recv:        map[uint32]*recvStats{},
		closed:      make(chan struct{}),
		refreshKick: make(chan struct{}, 1),
	}
	if err := c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Hello{Hello: &controlpb.Hello{
		Token: opts.Token, ClientVersion: opts.ClientVersion, Capabilities: opts.Capabilities}}}); err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	rd := wire.NewControlReader(ctrl)
	if dl, ok := ctx.Deadline(); ok {
		_ = ctrl.SetReadDeadline(dl)
	}
	first, err := rd.Read()
	_ = ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	if e := first.GetError(); e != nil {
		_ = conn.CloseWithError(0, "")
		return nil, &ServerError{e}
	}
	if c.welcome = first.GetWelcome(); c.welcome == nil {
		_ = conn.CloseWithError(1, "expected welcome")
		return nil, errors.New("client: expected Welcome")
	}
	if exp := c.welcome.GetTokenExpiresAt(); exp > 0 {
		c.expiresAt = time.Unix(exp, 0)
	}
	if c.opts.ReportInterval <= 0 {
		c.opts.ReportInterval = time.Second
	}
	go c.readLoop(rd)
	go c.acceptMedia()
	if !c.opts.NoReceiverReports {
		go c.reportLoop()
	}
	if c.opts.TokenSource != nil {
		go c.refreshLoop()
	}
	return c, nil
}

func (c *Client) Welcome() *controlpb.Welcome { return c.welcome }

// CapScreenAudio: screen share sound (docs/screen-audio.md). As a Hello
// capability the client handles SCREEN_AUDIO tracks; in Welcome the
// server relays them.
const CapScreenAudio = "screen-audio"

// ServerCan reports whether the server announced a capability in Welcome.
func (c *Client) ServerCan(capability string) bool {
	return slices.Contains(c.welcome.GetCapabilities(), capability)
}
func (c *Client) SessionID() uint32 { return c.welcome.GetSessionId() }

// Events delivers room events: ParticipantJoined/Left, TrackAdded/Removed,
// and errors not tied to a request. Closed when the connection ends.
func (c *Client) Events() <-chan *controlpb.ControlMessage { return c.events }

// Done is closed when the connection ends; Err says why.
func (c *Client) Done() <-chan struct{} { return c.closed }
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

func (c *Client) Close() error {
	return c.conn.CloseWithError(0, "bye")
}

func (c *Client) send(m *controlpb.ControlMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wire.WriteControl(c.ctrl, m)
}

// Join enters a room and returns its state (participants and tracks).
func (c *Client) Join(ctx context.Context, room string) (*controlpb.RoomState, error) {
	ch := make(chan *controlpb.ControlMessage, 1)
	c.mu.Lock()
	c.joinWait = ch
	c.mu.Unlock()
	if err := c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Join{Join: &controlpb.Join{Room: room}}}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if e := m.GetError(); e != nil {
			return nil, &ServerError{e}
		}
		c.mu.Lock()
		for _, t := range m.GetRoomState().GetTracks() {
			c.tracks[t.GetTrackId()] = t
		}
		c.mu.Unlock()
		return m.GetRoomState(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, ErrClosed
	}
}

// Leave leaves the room (tracks are unpublished by the server).
func (c *Client) Leave() error {
	c.mu.Lock()
	for id, t := range c.local {
		t.stop()
		delete(c.local, id)
	}
	clear(c.tracks)
	clear(c.recv)
	clear(c.subStates)
	c.mu.Unlock()
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Leave{Leave: &controlpb.Leave{}}})
}

// Subscribe asks for a track. maxLayer is the best layer wanted (e.g.
// what fits the tile); the server forwards the best layer at or below it
// that the connection can take, and reports it in SubscriptionState
// events. priority 0 = server default; 1..7 = QUIC urgency (lower first).
// Calling it again changes max layer/priority.
func (c *Client) Subscribe(trackID, maxLayer, priority uint32) error {
	c.mu.Lock()
	if c.recv[trackID] == nil {
		c.recv[trackID] = newRecvStats()
	}
	c.mu.Unlock()
	layer := maxLayer
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Subscribe{Subscribe: &controlpb.Subscribe{
		TrackId: trackID, Layer: layer, Priority: priority}}})
}

func (c *Client) Unsubscribe(trackID uint32) error {
	c.mu.Lock()
	delete(c.recv, trackID)
	delete(c.subStates, trackID)
	c.mu.Unlock()
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Unsubscribe{Unsubscribe: &controlpb.Unsubscribe{TrackId: trackID}}})
}

// Ping sends a ping (keepalive / RTT probe; the Pong is not surfaced).
func (c *Client) Ping(id uint64) error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Ping{Ping: &controlpb.Ping{Id: id}}})
}

// Publish announces a track and returns it once the server accepts it.
func (c *Client) Publish(ctx context.Context, kind controlpb.TrackKind, codec string, layers []*controlpb.Layer, opts ...PublishOptions) (*LocalTrack, error) {
	var popts PublishOptions
	if len(opts) > 0 {
		popts = opts[0]
	}
	ref := strconv.FormatUint(c.nextRef.Add(1), 10)
	ch := make(chan *controlpb.ControlMessage, 1)
	c.mu.Lock()
	c.pubWait[ref] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pubWait, ref)
		c.mu.Unlock()
	}()
	if err := c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Publish{Publish: &controlpb.Publish{
		ClientRef: ref, Kind: kind, Codec: codec, Layers: layers, ForTrackId: popts.ForTrack}}}); err != nil {
		return nil, err
	}
	var m *controlpb.ControlMessage
	select {
	case m = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, ErrClosed
	}
	if e := m.GetError(); e != nil {
		return nil, &ServerError{e}
	}
	t := newLocalTrack(c, m.GetPublishOk().GetTrackId(), kind, layers, popts)
	c.mu.Lock()
	c.local[t.id] = t
	early := c.earlyActive[t.id]
	delete(c.earlyActive, t.id)
	c.mu.Unlock()
	for layer, active := range early {
		t.setActive(layer, active)
	}
	return t, nil
}

func (c *Client) readLoop(rd *wire.ControlReader) {
	defer close(c.events)
	defer func() {
		c.mu.Lock()
		if c.closeErr == nil {
			c.closeErr = ErrClosed
		}
		c.mu.Unlock()
		close(c.closed)
	}()
	for {
		m, err := rd.Read()
		if err != nil {
			c.mu.Lock()
			c.closeErr = err
			c.mu.Unlock()
			return
		}
		switch x := m.Msg.(type) {
		case *controlpb.ControlMessage_RoomState:
			c.resolveJoin(m)
		case *controlpb.ControlMessage_PublishOk:
			c.resolvePublish(x.PublishOk.GetClientRef(), m)
		case *controlpb.ControlMessage_Error:
			if ref := x.Error.GetClientRef(); ref != "" && c.resolvePublish(ref, m) {
				continue
			}
			if x.Error.GetCode() == controlpb.Error_TOKEN_EXPIRED && !x.Error.GetFatal() {
				select {
				case c.refreshKick <- struct{}{}:
				default:
				}
			}
			if c.resolveRefresh(m) {
				continue
			}
			if c.resolveJoin(m) {
				continue
			}
			c.emit(m)
		case *controlpb.ControlMessage_KeyframeRequest:
			if t := c.localTrack(x.KeyframeRequest.GetTrackId()); t != nil {
				t.keyframeNeeded(x.KeyframeRequest.GetLayer())
			}
		case *controlpb.ControlMessage_LayerActive:
			la := x.LayerActive
			c.mu.Lock()
			t := c.local[la.GetTrackId()]
			if t == nil {
				if c.earlyActive == nil {
					c.earlyActive = map[uint32]map[uint32]bool{}
				}
				if c.earlyActive[la.GetTrackId()] == nil {
					c.earlyActive[la.GetTrackId()] = map[uint32]bool{}
				}
				c.earlyActive[la.GetTrackId()][la.GetLayer()] = la.GetActive()
			}
			c.mu.Unlock()
			if t != nil {
				t.setActive(la.GetLayer(), la.GetActive())
			}
		case *controlpb.ControlMessage_SubscriptionState:
			c.mu.Lock()
			if _, ok := c.recv[x.SubscriptionState.GetTrackId()]; ok {
				c.subStates[x.SubscriptionState.GetTrackId()] = x.SubscriptionState
			}
			c.mu.Unlock()
			c.emit(m)
		case *controlpb.ControlMessage_RefreshOk:
			c.mu.Lock()
			if exp := x.RefreshOk.GetTokenExpiresAt(); exp > 0 {
				c.expiresAt = time.Unix(exp, 0)
			}
			ch := c.refreshWait
			c.refreshWait = nil
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case *controlpb.ControlMessage_TrackAdded:
			c.mu.Lock()
			c.tracks[x.TrackAdded.GetTrack().GetTrackId()] = x.TrackAdded.GetTrack()
			c.mu.Unlock()
			c.emit(m)
		case *controlpb.ControlMessage_TrackRemoved:
			c.mu.Lock()
			delete(c.tracks, x.TrackRemoved.GetTrackId())
			delete(c.recv, x.TrackRemoved.GetTrackId())
			delete(c.subStates, x.TrackRemoved.GetTrackId())
			c.mu.Unlock()
			c.emit(m)
		case *controlpb.ControlMessage_Pong:
		default:
			c.emit(m)
		}
	}
}

func (c *Client) emit(m *controlpb.ControlMessage) {
	select {
	case c.events <- m:
	case <-c.conn.Context().Done():
	}
}

func (c *Client) resolveJoin(m *controlpb.ControlMessage) bool {
	c.mu.Lock()
	ch := c.joinWait
	c.joinWait = nil
	c.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- m
	return true
}

func (c *Client) resolvePublish(ref string, m *controlpb.ControlMessage) bool {
	c.mu.Lock()
	ch := c.pubWait[ref]
	delete(c.pubWait, ref)
	c.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- m
	return true
}

func (c *Client) acceptMedia() {
	ctx := c.conn.Context()
	for {
		st, err := c.conn.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		go c.readGroup(st)
	}
}

func (c *Client) readGroup(st *quic.ReceiveStream) {
	rd, err := wire.NewMediaReader(st, 0)
	if err != nil {
		st.CancelRead(wire.StreamCodeProtocol)
		return
	}
	h := rd.Header()
	first := true
	for {
		f, err := rd.Next()
		if err != nil {
			complete := errors.Is(err, io.EOF)
			if !complete {
				st.CancelRead(wire.StreamCodeGone)
			}
			if !complete {
				c.observeCut(h.TrackID)
			}
			if c.opts.OnGroupEnd != nil {
				c.opts.OnGroupEnd(h.TrackID, h.Layer, h.Group, complete)
			}
			return
		}
		c.observeFrame(h.TrackID, h.Layer, f)
		if c.opts.OnFrame != nil {
			c.opts.OnFrame(RemoteFrame{TrackID: h.TrackID, Layer: h.Layer, Group: h.Group, GroupStart: first, Frame: *f})
		}
		first = false
	}
}

func (c *Client) localTrack(id uint32) *LocalTrack {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local[id]
}

// SubscriptionState is the server's latest report on a subscription (nil
// until the first one).
func (c *Client) SubscriptionState(trackID uint32) *controlpb.SubscriptionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subStates[trackID]
}

// Track is a remote track's description (nil if unknown).
func (c *Client) Track(trackID uint32) *controlpb.Track {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tracks[trackID]
}

// TokenExpiresAt is when the session's token expires (zero = never).
func (c *Client) TokenExpiresAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiresAt
}

// Refresh sends a fresh token for this session (same user) and waits for
// the server to accept it.
func (c *Client) Refresh(ctx context.Context, token string) error {
	ch := make(chan *controlpb.ControlMessage, 1)
	c.mu.Lock()
	c.refreshWait = ch
	c.mu.Unlock()
	if err := c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Refresh{Refresh: &controlpb.Refresh{Token: token}}}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if e := m.GetError(); e != nil {
			return &ServerError{e}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
}

func (c *Client) resolveRefresh(m *controlpb.ControlMessage) bool {
	if !strings.HasPrefix(m.GetError().GetMessage(), "refresh:") {
		return false
	}
	c.mu.Lock()
	ch := c.refreshWait
	c.refreshWait = nil
	c.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- m
	return true
}

// refreshLoop renews the token a minute before it expires (or at 80% of a
// shorter lifetime), and right away when the server warns it has expired.
func (c *Client) refreshLoop() {
	for {
		exp := c.TokenExpiresAt()
		var wait <-chan time.Time
		if !exp.IsZero() {
			lead := time.Minute
			if life := time.Until(exp); life < 5*time.Minute {
				lead = life / 5
			}
			wait = time.After(max(time.Until(exp)-lead, 0))
		}
		select {
		case <-c.closed:
			return
		case <-wait:
		case <-c.refreshKick:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tok, err := c.opts.TokenSource(ctx)
		if err == nil {
			err = c.Refresh(ctx, tok)
		}
		cancel()
		if err != nil {
			select {
			case <-c.closed:
				return
			case <-time.After(5 * time.Second): // retry
			}
		}
	}
}
