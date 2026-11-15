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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
)

var (
	ErrClosed       = errors.New("client: closed")
	ErrNeedKeyframe = errors.New("client: a group must start with a keyframe")
	ErrNoLayer      = errors.New("client: no such layer")
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
}

// Client is one connection to quicvideod.
type Client struct {
	conn    *quic.Conn
	ctrl    *quic.Stream
	opts    Options
	welcome *controlpb.Welcome
	events  chan *controlpb.ControlMessage

	writeMu sync.Mutex

	mu       sync.Mutex
	joinWait chan *controlpb.ControlMessage
	pubWait  map[string]chan *controlpb.ControlMessage
	local    map[uint32]*LocalTrack
	nextRef  atomic.Uint64
	closed   chan struct{}
	closeErr error
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
		events:  make(chan *controlpb.ControlMessage, opts.EventBuffer),
		pubWait: map[string]chan *controlpb.ControlMessage{},
		local:   map[uint32]*LocalTrack{},
		closed:  make(chan struct{}),
	}
	if err := c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Hello{Hello: &controlpb.Hello{
		Token: opts.Token, ClientVersion: opts.ClientVersion}}}); err != nil {
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
	go c.readLoop(rd)
	go c.acceptMedia()
	return c, nil
}

func (c *Client) Welcome() *controlpb.Welcome { return c.welcome }
func (c *Client) SessionID() uint32           { return c.welcome.GetSessionId() }

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
		t.closeStreams()
		delete(c.local, id)
	}
	c.mu.Unlock()
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Leave{Leave: &controlpb.Leave{}}})
}

// Subscribe asks for a track's layer. priority 0 = server default; 1..7 =
// QUIC urgency (lower first). Calling it again switches layer/priority.
func (c *Client) Subscribe(trackID, layer, priority uint32) error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Subscribe{Subscribe: &controlpb.Subscribe{
		TrackId: trackID, Layer: layer, Priority: priority}}})
}

func (c *Client) Unsubscribe(trackID uint32) error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Unsubscribe{Unsubscribe: &controlpb.Unsubscribe{TrackId: trackID}}})
}

// Ping sends a ping (keepalive / RTT probe; the Pong is not surfaced).
func (c *Client) Ping(id uint64) error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Ping{Ping: &controlpb.Ping{Id: id}}})
}

// Publish announces a track and returns it once the server accepts it.
func (c *Client) Publish(ctx context.Context, kind controlpb.TrackKind, codec string, layers []*controlpb.Layer) (*LocalTrack, error) {
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
		ClientRef: ref, Kind: kind, Codec: codec, Layers: layers}}}); err != nil {
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
	urgency := int8(4)
	if kind == controlpb.TrackKind_SCREEN {
		urgency = 1
	}
	t := &LocalTrack{c: c, id: m.GetPublishOk().GetTrackId(), urgency: urgency, layers: map[uint32]*localLayer{},
		kfReq: make(chan uint32, 16)}
	for _, l := range layers {
		t.layers[l.GetId()] = &localLayer{}
	}
	c.mu.Lock()
	c.local[t.id] = t
	c.mu.Unlock()
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
			if c.resolveJoin(m) {
				continue
			}
			c.emit(m)
		case *controlpb.ControlMessage_KeyframeRequest:
			c.mu.Lock()
			t := c.local[x.KeyframeRequest.GetTrackId()]
			c.mu.Unlock()
			if t != nil {
				select {
				case t.kfReq <- x.KeyframeRequest.GetLayer():
				default:
				}
			}
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
			if c.opts.OnGroupEnd != nil {
				c.opts.OnGroupEnd(h.TrackID, h.Layer, h.Group, complete)
			}
			return
		}
		if c.opts.OnFrame != nil {
			c.opts.OnFrame(RemoteFrame{TrackID: h.TrackID, Layer: h.Layer, Group: h.Group, GroupStart: first, Frame: *f})
		}
		first = false
	}
}

// LocalTrack is a published track.
type LocalTrack struct {
	c       *Client
	id      uint32
	urgency int8
	layers  map[uint32]*localLayer
	kfReq   chan uint32
}

type localLayer struct {
	mu     sync.Mutex
	st     *quic.SendStream
	group  uint64
	hdrBuf []byte
}

func (t *LocalTrack) ID() uint32 { return t.id }

// KeyframeRequests delivers layer ids the server wants a keyframe on (a
// subscriber joined without a cached group, or one was dropped).
func (t *LocalTrack) KeyframeRequests() <-chan uint32 { return t.kfReq }

// WriteFrame sends one encoded frame on a layer. A keyframe starts a new
// group (a new stream); other frames continue the current one. If the
// server dropped the current group, it returns the write error and the
// next keyframe starts over. captureUS = 0 uses the current time.
func (t *LocalTrack) WriteFrame(layer uint32, keyframe bool, captureUS uint64, payload []byte) error {
	l := t.layers[layer]
	if l == nil {
		return ErrNoLayer
	}
	if captureUS == 0 {
		captureUS = uint64(time.Now().UnixMicro())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if keyframe {
		if l.st != nil {
			_ = l.st.Close()
		}
		st, err := t.c.conn.OpenUniStream()
		if err != nil {
			// Stream limit reached: wait for one (rare; the server allows 256).
			ctx, cancel := context.WithTimeout(t.c.conn.Context(), 2*time.Second)
			st, err = t.c.conn.OpenUniStreamSync(ctx)
			cancel()
			if err != nil {
				l.st = nil
				return err
			}
		}
		st.SetPriority(t.urgency, false)
		l.group++
		l.st = st
		l.hdrBuf = wire.AppendGroupHeader(l.hdrBuf[:0], wire.GroupHeader{TrackID: t.id, Layer: layer, Group: l.group})
		if _, err := st.Write(l.hdrBuf); err != nil {
			l.st = nil
			return err
		}
	} else if l.st == nil {
		return ErrNeedKeyframe
	}
	f := wire.Frame{Keyframe: keyframe, CaptureUS: captureUS, Payload: payload}
	l.hdrBuf = wire.AppendFrameHeader(l.hdrBuf[:0], &f)
	_, err := l.st.Write(l.hdrBuf)
	if err == nil {
		_, err = l.st.Write(payload)
	}
	if err != nil {
		l.st.CancelWrite(wire.StreamCodeGone)
		l.st = nil
	}
	return err
}

// EndGroup closes the current group on a layer without starting a new one
// (e.g. the encoder is paused).
func (t *LocalTrack) EndGroup(layer uint32) {
	if l := t.layers[layer]; l != nil {
		l.mu.Lock()
		if l.st != nil {
			_ = l.st.Close()
			l.st = nil
		}
		l.mu.Unlock()
	}
}

func (t *LocalTrack) closeStreams() {
	for _, l := range t.layers {
		l.mu.Lock()
		if l.st != nil {
			l.st.CancelWrite(wire.StreamCodeGone)
			l.st = nil
		}
		l.mu.Unlock()
	}
}

// Unpublish stops the track.
func (t *LocalTrack) Unpublish() error {
	t.closeStreams()
	t.c.mu.Lock()
	delete(t.c.local, t.id)
	t.c.mu.Unlock()
	return t.c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Unpublish{Unpublish: &controlpb.Unpublish{TrackId: t.id}}})
}
