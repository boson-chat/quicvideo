//go:build linux && cgo

package media

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// The xdg-desktop-portal ScreenCast flow (works on Wayland and X11
// desktops that run a portal):
//
//	CreateSession → SelectSources (monitor/window, cursor, persist) →
//	Start (the user picks in the system dialog) → OpenPipeWireRemote
//
// Each step answers through a Request object's Response signal. The
// restore token from Start is saved, so the next share reuses the choice
// without a dialog (if the portal allows it).

const (
	portalDest = "org.freedesktop.portal.Desktop"
	portalPath = "/org/freedesktop/portal/desktop"
	screenCast = "org.freedesktop.portal.ScreenCast"
)

// ErrPortalCancelled: the user closed the screen-share picker.
var ErrPortalCancelled = errors.New("media: screen sharing was cancelled in the system picker")

type portalSession struct {
	conn          *dbus.Conn
	handle        dbus.ObjectPath
	fd            int
	node          uint32
	width, height int
	restored      bool // a stored restore token was offered
}

func (p *portalSession) close() {
	if p.conn == nil {
		return
	}
	_ = p.conn.Object(portalDest, p.handle).Call("org.freedesktop.portal.Session.Close", 0).Err
	p.conn.Close()
	p.conn = nil
}

// tokenFile is where the portal's restore token for a kind of source
// ("monitor" or "window") is kept, per user: $QUICVIDEO_CONFIG_DIR, or the
// XDG config dir (~/.config/quicvideo).
func tokenFile(kind string) string {
	name := "screencast-restore-token-" + kind
	if d := os.Getenv("QUICVIDEO_CONFIG_DIR"); d != "" {
		return filepath.Join(d, name)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "quicvideo", name)
}

// request calls a portal method that answers with a Response signal and
// waits (up to timeout: a human may be picking) for its results.
func request(conn *dbus.Conn, method string, timeout time.Duration, args ...any) (map[string]dbus.Variant, error) {
	token := fmt.Sprintf("qv%d", rand.Uint32())
	sender := strings.ReplaceAll(strings.TrimPrefix(conn.Names()[0], ":"), ".", "_")
	path := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/portal/desktop/request/%s/%s", sender, token))
	ch := make(chan *dbus.Signal, 4)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(path), dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response")); err != nil {
		return nil, err
	}
	// The options dict is the last argument: add our handle_token to it.
	opts := args[len(args)-1].(map[string]dbus.Variant)
	opts["handle_token"] = dbus.MakeVariant(token)
	if err := conn.Object(portalDest, portalPath).Call(screenCast+"."+method, 0, args...).Err; err != nil {
		return nil, fmt.Errorf("media: portal %s: %w", method, err)
	}
	deadline := time.After(timeout)
	for {
		select {
		case s := <-ch:
			if s.Path != path || len(s.Body) < 2 {
				continue
			}
			code, _ := s.Body[0].(uint32)
			results, _ := s.Body[1].(map[string]dbus.Variant)
			switch code {
			case 0:
				return results, nil
			case 1:
				return nil, ErrPortalCancelled
			default:
				return nil, fmt.Errorf("media: portal %s failed", method)
			}
		case <-deadline:
			return nil, fmt.Errorf("media: portal %s: no answer in %v", method, timeout)
		}
	}
}

func portalScreenCast(window, cursor bool) (*portalSession, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("media: no D-Bus session (screen sharing needs a desktop session): %w", err)
	}
	ps := &portalSession{conn: conn}
	ok := false
	defer func() {
		if !ok {
			ps.close()
		}
	}()
	res, err := request(conn, "CreateSession", 10*time.Second, map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(fmt.Sprintf("qvs%d", rand.Uint32())),
	})
	if err != nil {
		return nil, err
	}
	h, _ := res["session_handle"].Value().(string)
	ps.handle = dbus.ObjectPath(h)

	types, kind := uint32(1), "monitor"
	if window {
		types, kind = 2, "window"
	}
	cursorMode := uint32(1) // hidden
	if cursor {
		cursorMode = 2 // embedded
	}
	sel := map[string]dbus.Variant{
		"types":        dbus.MakeVariant(types),
		"multiple":     dbus.MakeVariant(false),
		"cursor_mode":  dbus.MakeVariant(cursorMode),
		"persist_mode": dbus.MakeVariant(uint32(2)), // until revoked
	}
	// A stored token restores the user's earlier choice without a dialog.
	// If the portal rejects it (revoked, source gone), it shows the picker
	// and Start returns a fresh token, saved below.
	restored := false
	if tf := tokenFile(kind); tf != "" {
		if b, err := os.ReadFile(tf); err == nil && len(bytes.TrimSpace(b)) > 0 {
			sel["restore_token"] = dbus.MakeVariant(string(bytes.TrimSpace(b)))
			restored = true
		}
	}
	// Some portals (Hyprland) show their picker here rather than in Start.
	if _, err := request(conn, "SelectSources", 2*time.Minute, ps.handle, sel); err != nil {
		return nil, err
	}
	res, err = request(conn, "Start", 2*time.Minute, ps.handle, "", map[string]dbus.Variant{})
	if err != nil {
		return nil, err
	}
	// Tokens are single-use: the portal hands out a new one every time.
	if t, ok := res["restore_token"].Value().(string); ok && t != "" {
		if tf := tokenFile(kind); tf != "" {
			_ = os.MkdirAll(filepath.Dir(tf), 0o700)
			_ = os.WriteFile(tf, []byte(t), 0o600)
		}
	}
	ps.restored = restored
	if restored {
		fmt.Fprintf(os.Stderr, "media: screen share: offered the saved %s choice (restore token)\n", kind)
	} else {
		fmt.Fprintf(os.Stderr, "media: screen share: no saved %s choice, the system picker asked\n", kind)
	}
	streams, ok2 := res["streams"].Value().([][]any)
	if !ok2 || len(streams) == 0 {
		return nil, errors.New("media: the portal returned no stream")
	}
	ps.node, _ = streams[0][0].(uint32)
	if props, ok := streams[0][1].(map[string]dbus.Variant); ok {
		if sz, ok := props["size"].Value().([]any); ok && len(sz) == 2 {
			w, _ := sz[0].(int32)
			h, _ := sz[1].(int32)
			ps.width, ps.height = int(w), int(h)
		}
	}
	var fd dbus.UnixFD
	if err := conn.Object(portalDest, portalPath).Call(screenCast+".OpenPipeWireRemote", 0, ps.handle,
		map[string]dbus.Variant{}).Store(&fd); err != nil {
		return nil, fmt.Errorf("media: portal OpenPipeWireRemote: %w", err)
	}
	ps.fd = int(fd)
	ok = true
	return ps, nil
}
