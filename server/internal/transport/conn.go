package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/boson-chat/quicvideo/server/internal/relay"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// mconn is a client connection carrying the quicvideo protocol: native
// QUIC (ALPN boson-video/1) or a WebTransport session (browsers). Both
// carry the same streams: one bidirectional control stream opened by the
// client, unidirectional media streams both ways.
type mconn interface {
	AcceptControl(ctx context.Context) (ctrlStream, error)
	AcceptGroup(ctx context.Context) (groupStream, error)
	OpenGroup(ctx context.Context, urgency int8) (relay.GroupWriter, error)
	Close(code uint32, msg string)
	Context() context.Context
	RemoteAddr() net.Addr
	// Stats returns transport statistics when available.
	Stats() (relay.NetStats, bool)
	Kind() string
}

type ctrlStream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
}

type groupStream interface {
	io.Reader
	CancelRead(code uint64)
}

// isReset reports whether err is the peer resetting a stream (an aborted
// group, not a protocol violation).
func isReset(err error) bool {
	var qe *quic.StreamError
	var we *webtransport.StreamError
	return errors.As(err, &qe) || errors.As(err, &we)
}

// ---- native QUIC -----------------------------------------------------------

type quicConn struct{ c *quic.Conn }

func (q quicConn) Kind() string { return "quic" }

func (q quicConn) AcceptControl(ctx context.Context) (ctrlStream, error) {
	st, err := q.c.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	st.SetPriority(0, false)
	return st, nil
}

type quicRecv struct{ *quic.ReceiveStream }

func (r quicRecv) CancelRead(code uint64) { r.ReceiveStream.CancelRead(quic.StreamErrorCode(code)) }

func (q quicConn) AcceptGroup(ctx context.Context) (groupStream, error) {
	st, err := q.c.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return quicRecv{st}, nil
}

type quicSend struct{ *quic.SendStream }

func (s quicSend) CancelWrite(code uint64) { s.SendStream.CancelWrite(quic.StreamErrorCode(code)) }

func (q quicConn) OpenGroup(ctx context.Context, urgency int8) (relay.GroupWriter, error) {
	st, err := q.c.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	st.SetPriority(urgency, false)
	return quicSend{st}, nil
}

func (q quicConn) Close(code uint32, msg string) {
	_ = q.c.CloseWithError(quic.ApplicationErrorCode(code), msg)
}
func (q quicConn) Context() context.Context { return q.c.Context() }
func (q quicConn) RemoteAddr() net.Addr     { return q.c.RemoteAddr() }
func (q quicConn) Stats() (relay.NetStats, bool) {
	st := q.c.ConnectionStats()
	return relay.NetStats{SmoothedRTT: st.SmoothedRTT, MinRTT: st.MinRTT, PacketsSent: st.PacketsSent, PacketsLost: st.PacketsLost}, true
}

// ---- WebTransport ----------------------------------------------------------

type wtConn struct{ s *webtransport.Session }

func (w wtConn) Kind() string { return "webtransport" }

type wtStream struct{ *webtransport.Stream }

func (w wtConn) AcceptControl(ctx context.Context) (ctrlStream, error) {
	st, err := w.s.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return wtStream{st}, nil
}

type wtRecv struct{ *webtransport.ReceiveStream }

func (r wtRecv) CancelRead(code uint64) {
	r.ReceiveStream.CancelRead(webtransport.StreamErrorCode(code))
}

func (w wtConn) AcceptGroup(ctx context.Context) (groupStream, error) {
	st, err := w.s.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return wtRecv{st}, nil
}

type wtSend struct{ *webtransport.SendStream }

func (s wtSend) CancelWrite(code uint64) {
	s.SendStream.CancelWrite(webtransport.StreamErrorCode(code))
}

func (w wtConn) OpenGroup(ctx context.Context, urgency int8) (relay.GroupWriter, error) {
	st, err := w.s.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	st.SetPriority(urgency, false)
	return wtSend{st}, nil
}

func (w wtConn) Close(code uint32, msg string) {
	_ = w.s.CloseWithError(webtransport.SessionErrorCode(code), msg)
}
func (w wtConn) Context() context.Context      { return w.s.Context() }
func (w wtConn) RemoteAddr() net.Addr          { return w.s.RemoteAddr() }
func (w wtConn) Stats() (relay.NetStats, bool) { return relay.NetStats{}, false }
