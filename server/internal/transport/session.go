package transport

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/internal/logger"
	"github.com/boson-chat/quicvideo/server/internal/relay"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/boson-chat/quicvideo/server/metrics"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
)

// controlQueue is how many outgoing control messages may wait before the
// client counts as a slow consumer and is disconnected.
const controlQueue = 512

type session struct {
	s    *Server
	conn *quic.Conn
	id   uint32

	ident  *auth.Identity // read loop only
	out    chan *controlpb.ControlMessage
	expiry chan time.Time // new token expiry (Refresh) → watchExpiry

	mu     sync.Mutex
	member *relay.Member
}

func newSession(s *Server, conn *quic.Conn, id uint32) *session {
	return &session{s: s, conn: conn, id: id, out: make(chan *controlpb.ControlMessage, controlQueue),
		expiry: make(chan time.Time, 1)}
}

// ---- relay.Peer ------------------------------------------------------------

func (ss *session) ID() uint32 { return ss.id }

func (ss *session) SendControl(m *controlpb.ControlMessage) {
	select {
	case ss.out <- m:
	default:
		_ = ss.conn.CloseWithError(CodeSlow, "control queue full")
	}
}

// NetStats implements relay.StatsPeer.
func (ss *session) NetStats() relay.NetStats {
	st := ss.conn.ConnectionStats()
	return relay.NetStats{SmoothedRTT: st.SmoothedRTT, MinRTT: st.MinRTT, PacketsSent: st.PacketsSent, PacketsLost: st.PacketsLost}
}

type sendStream struct{ *quic.SendStream }

func (s sendStream) CancelWrite(code uint64) { s.SendStream.CancelWrite(quic.StreamErrorCode(code)) }

func (ss *session) OpenGroup(ctx context.Context, urgency int8) (relay.GroupWriter, error) {
	st, err := ss.conn.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	st.SetPriority(urgency, false)
	return sendStream{st}, nil
}

// ---- session ---------------------------------------------------------------

func (ss *session) run(ctx context.Context) error {
	log := logger.FromCtx(ctx)
	m := ss.s.metrics

	actx, cancel := context.WithDeadline(ctx, ss.s.helloDeadline())
	ctrl, err := ss.conn.AcceptStream(actx)
	cancel()
	if err != nil {
		m.Handshake(metrics.HandshakeNoStream)
		_ = ss.conn.CloseWithError(CodeProtocol, "no control stream")
		return err
	}
	ctrl.SetPriority(0, false)
	_ = ctrl.SetReadDeadline(ss.s.helloDeadline())
	rd := wire.NewControlReader(ctrl)
	first, err := rd.Read()
	if err != nil {
		m.Handshake(metrics.HandshakeNoHello)
		_ = ss.conn.CloseWithError(CodeProtocol, "no hello")
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		m.Handshake(metrics.HandshakeProtocol)
		_ = ss.conn.CloseWithError(CodeProtocol, "expected hello")
		return errors.New("first message is not hello")
	}
	ident, err := ss.s.auth.Verify(ctx, hello.GetToken())
	if err != nil {
		code := controlpb.Error_UNAUTHENTICATED
		outcome := metrics.HandshakeBadToken
		if errors.Is(err, auth.ErrTokenExpired) {
			code, outcome = controlpb.Error_TOKEN_EXPIRED, metrics.HandshakeExpired
		}
		m.Handshake(outcome)
		_ = wire.WriteControl(ctrl, errMsg(code, err.Error(), true, ""))
		_ = ctrl.Close()
		// Give the Error time to arrive; the client closes when it reads it.
		select {
		case <-ss.conn.Context().Done():
		case <-time.After(time.Second):
		}
		_ = ss.conn.CloseWithError(CodeAuth, "unauthenticated")
		return err
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	ss.ident = ident
	m.Handshake(metrics.HandshakeOK)
	m.SessionOpened()
	defer m.SessionClosed()
	log.Debug().Str("user", ident.UserID).Uint32("session", ss.id).Msg("authenticated")

	rc := ss.s.cfg.RelayConfig
	ss.out <- &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Welcome{Welcome: &controlpb.Welcome{
		SessionId:       ss.id,
		MaxFrameSize:    uint32(rc.MaxFrameSize),
		MaxTracks:       uint32(rc.MaxTracksPerSession),
		MaxLayers:       uint32(rc.MaxLayers),
		MaxTrackBitrate: rc.MaxTrackBitrate,
		TokenExpiresAt:  unix(ident.ExpiresAt),
	}}}

	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	go ss.writeLoop(ctx, ctrl)
	go ss.acceptMedia(ctx)
	go ss.watchExpiry(ctx, ident.ExpiresAt)
	defer ss.leave("disconnected")

	for {
		msg, err := rd.Read()
		if err != nil {
			return err
		}
		if fatal := ss.handle(msg); fatal {
			_ = ss.conn.CloseWithError(CodeProtocol, "protocol error")
			return errors.New("protocol error")
		}
	}
}

func (ss *session) writeLoop(ctx context.Context, ctrl *quic.Stream) {
	var buf []byte
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ss.out:
			buf = buf[:0]
			var err error
			if buf, err = wire.AppendControl(buf, m); err != nil {
				continue
			}
			// Batch whatever else is waiting into the same write.
		drain:
			for len(buf) < 64<<10 {
				select {
				case m := <-ss.out:
					buf, _ = wire.AppendControl(buf, m)
				default:
					break drain
				}
			}
			if _, err := ctrl.Write(buf); err != nil {
				_ = ss.conn.CloseWithError(CodeProtocol, "control write failed")
				return
			}
		}
	}
}

// handle processes one control message; true = fatal protocol error.
func (ss *session) handle(msg *controlpb.ControlMessage) bool {
	switch x := msg.Msg.(type) {
	case *controlpb.ControlMessage_Join:
		ss.join(x.Join.GetRoom())
	case *controlpb.ControlMessage_Leave:
		ss.leave("left")
	case *controlpb.ControlMessage_Publish:
		mem := ss.current()
		if mem == nil {
			ss.SendControl(errMsg(controlpb.Error_NOT_IN_ROOM, "join a room first", false, x.Publish.GetClientRef()))
			return false
		}
		// On success the relay sends PublishOK itself (before announcing the track).
		if _, err := mem.Publish(x.Publish); err != nil {
			ss.SendControl(errMsg(errCode(err), err.Error(), false, x.Publish.GetClientRef()))
			return false
		}
	case *controlpb.ControlMessage_Unpublish:
		ss.withMember(func(m *relay.Member) error { return m.Unpublish(x.Unpublish.GetTrackId()) })
	case *controlpb.ControlMessage_Subscribe:
		s := x.Subscribe
		ss.withMember(func(m *relay.Member) error { return m.Subscribe(s.GetTrackId(), s.GetLayer(), s.GetPriority()) })
	case *controlpb.ControlMessage_Unsubscribe:
		ss.withMember(func(m *relay.Member) error { return m.Unsubscribe(x.Unsubscribe.GetTrackId()) })
	case *controlpb.ControlMessage_ReceiverReport:
		if mem := ss.current(); mem != nil {
			mem.OnReceiverReport(x.ReceiverReport)
		}
	case *controlpb.ControlMessage_Refresh:
		ss.refresh(x.Refresh.GetToken())
	case *controlpb.ControlMessage_Ping:
		ss.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Pong{Pong: &controlpb.Pong{Id: x.Ping.GetId()}}})
	case *controlpb.ControlMessage_Hello:
		return true
	default:
		// Server→client messages or unknown future ones: ignore.
	}
	return false
}

func (ss *session) current() *relay.Member {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.member
}

func (ss *session) withMember(f func(*relay.Member) error) {
	mem := ss.current()
	if mem == nil {
		ss.SendControl(errMsg(controlpb.Error_NOT_IN_ROOM, "join a room first", false, ""))
		return
	}
	if err := f(mem); err != nil {
		ss.SendControl(errMsg(errCode(err), err.Error(), false, ""))
	}
}

func (ss *session) join(room string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.member != nil {
		ss.SendControl(errMsg(controlpb.Error_PROTOCOL, "already in a room", false, ""))
		return
	}
	if !ss.ident.CanJoin(room) {
		ss.SendControl(errMsg(controlpb.Error_FORBIDDEN, "token does not allow this room", false, ""))
		return
	}
	mem, state, err := ss.s.relay.Join(ss, relay.Participant{
		UserID: ss.ident.UserID, DisplayName: ss.ident.DisplayName, CanPublish: ss.ident.CanPublish(),
	}, room)
	if err != nil {
		ss.SendControl(errMsg(errCode(err), err.Error(), false, ""))
		return
	}
	ss.member = mem
	ss.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_RoomState{RoomState: state}})
}

func (ss *session) leave(reason string) {
	ss.mu.Lock()
	mem := ss.member
	ss.member = nil
	ss.mu.Unlock()
	if mem != nil {
		mem.Leave(reason)
	}
}

// acceptMedia takes the client's media streams (one per group it publishes).
func (ss *session) acceptMedia(ctx context.Context) {
	for {
		st, err := ss.conn.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		go ss.ingest(st)
	}
}

func (ss *session) ingest(st *quic.ReceiveStream) {
	m := ss.s.metrics
	mem := ss.current()
	if mem == nil {
		st.CancelRead(wire.StreamCodeGone)
		return
	}
	rd, err := wire.NewMediaReader(st, ss.s.cfg.RelayConfig.MaxFrameSize)
	if err != nil {
		st.CancelRead(wire.StreamCodeProtocol)
		return
	}
	in, err := mem.OpenGroup(rd.Header())
	if err != nil {
		code := wire.StreamCodeGone
		if errors.Is(err, relay.ErrStale) {
			code = wire.StreamCodeStale
		}
		st.CancelRead(quic.StreamErrorCode(code))
		return
	}
	for {
		f, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				in.End(false)
				return
			}
			var se *quic.StreamError
			if !errors.As(err, &se) {
				st.CancelRead(wire.StreamCodeProtocol)
			}
			in.End(true)
			return
		}
		if err := in.Frame(f); err != nil {
			code := wire.StreamCodeDropped
			switch {
			case errors.Is(err, relay.ErrStale):
				code = wire.StreamCodeStale
			case errors.Is(err, relay.ErrGone):
				code = wire.StreamCodeGone
			case errors.Is(err, relay.ErrRateLimited):
				m.GroupDropped(metrics.DropRateLimit)
			}
			st.CancelRead(quic.StreamErrorCode(code))
			in.End(true)
			return
		}
	}
}

// refresh swaps in a new token for the same user.
func (ss *session) refresh(token string) {
	id, err := ss.s.auth.Verify(context.Background(), token)
	if err != nil {
		code := controlpb.Error_UNAUTHENTICATED
		if errors.Is(err, auth.ErrTokenExpired) {
			code = controlpb.Error_TOKEN_EXPIRED
		}
		ss.SendControl(errMsg(code, "refresh: "+err.Error(), false, ""))
		return
	}
	if id.UserID != ss.ident.UserID {
		ss.SendControl(errMsg(controlpb.Error_FORBIDDEN, "refresh: token is for another user", false, ""))
		return
	}
	ss.ident = id
	ss.s.metrics.TokenRefreshed()
	if mem := ss.current(); mem != nil && !id.CanJoin(mem.Room()) {
		ss.leave("token no longer allows this room")
		ss.SendControl(errMsg(controlpb.Error_FORBIDDEN, "new token does not allow this room", false, ""))
	}
	select {
	case <-ss.expiry:
	default:
	}
	ss.expiry <- id.ExpiresAt
	ss.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_RefreshOk{
		RefreshOk: &controlpb.RefreshOK{TokenExpiresAt: unix(id.ExpiresAt)}}})
}

// watchExpiry warns when the token expires and closes the session after
// TOKEN_GRACE unless a Refresh arrives.
func (ss *session) watchExpiry(ctx context.Context, exp time.Time) {
	grace := ss.s.cfg.AuthConfig.TokenGrace
	var timer *time.Timer
	var fire <-chan time.Time
	warned := false
	arm := func(at time.Time) {
		if timer != nil {
			timer.Stop()
		}
		fire = nil
		if !at.IsZero() {
			timer = time.NewTimer(time.Until(at))
			fire = timer.C
		}
	}
	arm(exp)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case exp = <-ss.expiry:
			warned = false
			arm(exp)
		case <-fire:
			if !warned {
				warned = true
				ss.s.metrics.TokenExpired()
				ss.SendControl(errMsg(controlpb.Error_TOKEN_EXPIRED,
					"token expired: send Refresh within "+grace.String(), false, ""))
				arm(time.Now().Add(grace))
				continue
			}
			ss.SendControl(errMsg(controlpb.Error_TOKEN_EXPIRED, "token expired", true, ""))
			// Let the error go out before closing.
			time.AfterFunc(500*time.Millisecond, func() { _ = ss.conn.CloseWithError(CodeAuth, "token expired") })
			return
		}
	}
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func errMsg(code controlpb.Error_Code, msg string, fatal bool, ref string) *controlpb.ControlMessage {
	return &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Error{Error: &controlpb.Error{
		Code: code, Message: msg, Fatal: fatal, ClientRef: ref}}}
}

func errCode(err error) controlpb.Error_Code {
	switch {
	case errors.Is(err, relay.ErrRoomFull):
		return controlpb.Error_ROOM_FULL
	case errors.Is(err, relay.ErrNotInRoom):
		return controlpb.Error_NOT_IN_ROOM
	case errors.Is(err, relay.ErrLimit):
		return controlpb.Error_LIMIT
	case errors.Is(err, relay.ErrTrackNotFound):
		return controlpb.Error_TRACK_NOT_FOUND
	case errors.Is(err, relay.ErrNotOwner):
		return controlpb.Error_FORBIDDEN
	default:
		return controlpb.Error_PROTOCOL
	}
}
