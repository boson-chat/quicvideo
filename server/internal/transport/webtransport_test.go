package transport_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/config"
	"github.com/boson-chat/quicvideo/server/internal/origins"
	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/boson-chat/quicvideo/server/internal/transport"
	"github.com/boson-chat/quicvideo/wire"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// wtClient speaks the quicvideo protocol over a WebTransport session, the
// way the browser client does.
type wtClient struct {
	sess *webtransport.Session
	ctrl *webtransport.Stream
	rd   *wire.ControlReader
}

func dialWT(t *testing.T, addr, token string) *wtClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	_, sess, err := d.Dial(ctx, fmt.Sprintf("https://%s%s", addr, transport.WebTransportPath), nil)
	if err != nil {
		t.Fatalf("webtransport dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	ctrl, err := sess.OpenStreamSync(ctx)
	must(t, err)
	c := &wtClient{sess: sess, ctrl: ctrl, rd: wire.NewControlReader(ctrl)}
	c.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Hello{Hello: &controlpb.Hello{Token: token}}})
	if c.next(t, func(m *controlpb.ControlMessage) bool { return m.GetWelcome() != nil }) == nil {
		t.Fatal("no Welcome over WebTransport")
	}
	return c
}

func (c *wtClient) send(t *testing.T, m *controlpb.ControlMessage) {
	t.Helper()
	must(t, wire.WriteControl(c.ctrl, m))
}

func (c *wtClient) next(t *testing.T, pred func(*controlpb.ControlMessage) bool) *controlpb.ControlMessage {
	t.Helper()
	_ = c.ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		m, err := c.rd.Read()
		if err != nil {
			t.Fatalf("control read: %v", err)
		}
		if pred(m) {
			return m
		}
	}
}

func TestWebTransportSession(t *testing.T) {
	addr := startServer(t, auth.NewInsecureAuthService())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A native publisher.
	alice := dial(t, addr, "alice", nil)
	_, err := alice.Join(ctx, "call")
	must(t, err)
	track, err := alice.Publish(ctx, controlpb.TrackKind_CAMERA, "h264", []*controlpb.Layer{{Id: 0, Width: 320, Height: 180, Fps: 15, Bitrate: 200_000}})
	must(t, err)

	// A browser-style viewer over WebTransport.
	web := dialWT(t, addr, "web")
	web.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Join{Join: &controlpb.Join{Room: "call"}}})
	st := web.next(t, func(m *controlpb.ControlMessage) bool { return m.GetRoomState() != nil }).GetRoomState()
	if len(st.GetTracks()) != 1 {
		t.Fatalf("room state %v", st)
	}
	web.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Subscribe{Subscribe: &controlpb.Subscribe{TrackId: track.ID(), Layer: 0}}})
	waitActive(t, track, 0, true)
	must(t, track.WriteFrame(0, true, 0, payload('K', 3000)))
	must(t, track.WriteFrame(0, false, 0, payload('p', 100)))

	gs, err := web.sess.AcceptUniStream(ctx)
	must(t, err)
	mr, err := wire.NewMediaReader(gs, 0)
	must(t, err)
	if h := mr.Header(); h.TrackID != track.ID() || h.Layer != 0 {
		t.Fatalf("group header %+v", h)
	}
	f1, err := mr.Next()
	must(t, err)
	f2, err := mr.Next()
	must(t, err)
	if !f1.Keyframe || len(f1.Payload) != 3000 || f2.Payload[0] != 'p' {
		t.Fatalf("frames over WebTransport: %v/%d, %q", f1.Keyframe, len(f1.Payload), f2.Payload[:1])
	}

	// Publishing from the WebTransport side; a native subscriber receives it.
	rx := newRecv()
	bob := dial(t, addr, "bob", rx.on)
	_, err = bob.Join(ctx, "call")
	must(t, err)
	web.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Publish{Publish: &controlpb.Publish{
		ClientRef: "1", Kind: controlpb.TrackKind_SCREEN, Codec: "h264", Layers: []*controlpb.Layer{{Id: 0, Width: 1280, Height: 720, Fps: 5, Bitrate: 300_000}}}}})
	ok := web.next(t, func(m *controlpb.ControlMessage) bool { return m.GetPublishOk() != nil }).GetPublishOk()
	must(t, bob.Subscribe(ok.GetTrackId(), 0, 0))
	web.next(t, func(m *controlpb.ControlMessage) bool { return m.GetLayerActive().GetActive() })
	us, err := web.sess.OpenUniStreamSync(ctx)
	must(t, err)
	b := wire.AppendGroupHeader(nil, wire.GroupHeader{TrackID: ok.GetTrackId(), Layer: 0, Group: 1})
	b = wire.AppendFrame(b, &wire.Frame{Keyframe: true, CaptureUS: uint64(time.Now().UnixMicro()), Payload: bytes.Repeat([]byte{'W'}, 5000)})
	_, err = us.Write(b)
	must(t, err)
	must(t, us.Close())
	f := rx.next(t)
	if f.TrackID != ok.GetTrackId() || !f.Keyframe || len(f.Payload) != 5000 || f.Payload[0] != 'W' {
		t.Fatalf("native subscriber got track %d key %v len %d", f.TrackID, f.Keyframe, len(f.Payload))
	}
	_ = io.EOF
}

// A page from an origin WEBTRANSPORT_ORIGINS doesn't list gets a 403, not
// an empty 200 a browser would take for an open session.
func TestWebTransportOriginRefused(t *testing.T) {
	addr := startServerCfg(t, auth.NewInsecureAuthService(), func(c *config.Config) {
		c.QUICConfig.WebTransportOrigins = "https://chat.example.com"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	url := fmt.Sprintf("https://%s%s", addr, transport.WebTransportPath)
	res, _, err := d.Dial(ctx, url, http.Header{"Origin": {"https://evil.example.com"}})
	if err == nil {
		t.Fatal("a session from a refused origin opened")
	}
	if res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("refused origin: response %v, err %v; want 403", res, err)
	}
	res, sess, err := d.Dial(ctx, url, http.Header{"Origin": {"https://chat.example.com"}})
	if err != nil {
		t.Fatalf("allowed origin: %v (%v)", err, res)
	}
	_ = sess.CloseWithError(0, "")
}

// WEBTRANSPORT_ORIGINS=directory,<own>: the own origin at once, the auth
// service's list once fetched, others refused with 403.
func TestWebTransportDirectoryOrigins(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"origins":["https://chat.other.example.com"]}`))
	}))
	defer authSrv.Close()
	f := origins.NewFetcher(authSrv.URL, time.Hour, nil)
	p, err := origins.Parse("directory,https://chat.example.com", f)
	must(t, err)
	addr := startServerOrigins(t, auth.NewInsecureAuthService(), nil, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	url := fmt.Sprintf("https://%s%s", addr, transport.WebTransportPath)
	dial := func(origin string) (int, error) {
		res, sess, err := d.Dial(ctx, url, http.Header{"Origin": {origin}})
		if err == nil {
			_ = sess.CloseWithError(0, "")
			return 200, nil
		}
		if res == nil {
			return 0, err
		}
		return res.StatusCode, err
	}
	if code, _ := dial("https://chat.other.example.com"); code != http.StatusForbidden {
		t.Fatalf("before the fetch: %d", code)
	}
	if code, err := dial("https://Chat.Example.com"); code != 200 {
		t.Fatalf("own origin: %d %v", code, err)
	}
	if _, err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if code, err := dial("https://chat.other.example.com"); code != 200 {
		t.Fatalf("listed origin: %d %v", code, err)
	}
	if code, _ := dial("https://evil.example.com"); code != http.StatusForbidden {
		t.Fatalf("unlisted origin: %d", code)
	}
}
