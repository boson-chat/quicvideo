package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"image"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/client/media"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

// session is the call: connection, remote tracks (decode → latest RGBA
// picture per track), subscriptions that follow the layout, and our own
// published camera/screen. It knows nothing about the UI toolkit; the UI
// reads snapshots of it and calls its methods.
type session struct {
	c          *client.Client
	invalidate func() // redraw the window

	mu        sync.Mutex
	remotes   map[uint32]*remote
	order     []uint32
	names     map[uint32]string
	pinned    uint32
	camera    *localShare
	screen    *localShare
	preview   *image.RGBA // our camera, small
	status    string
	lastTiles []Tile
	scale     float64
}

type remote struct {
	track      *controlpb.Track
	frames     chan client.RemoteFrame
	dec        media.VideoDecoder
	subscribed bool
	maxLayer   uint32
	tileW      int // device pixels, for scaling decoded pictures
	state      *controlpb.SubscriptionState
	img        *image.RGBA // latest decoded picture
	decoded    int
}

type localShare struct {
	src   media.Source
	pub   *media.Publisher
	track *client.LocalTrack
	name  string
}

func newSession(invalidate func()) *session {
	return &session{remotes: map[uint32]*remote{}, names: map[uint32]string{}, invalidate: invalidate, scale: 1}
}

func (s *session) setStatus(format string, args ...any) {
	s.mu.Lock()
	s.status = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	log.Print(s.status)
	s.invalidate()
}

func (s *session) run(ctx context.Context, addr, token, room string, insecure bool) error {
	c, err := client.Dial(ctx, addr, client.Options{
		Token: token, ClientVersion: "viewer/dev", TLS: &tls.Config{InsecureSkipVerify: insecure},
		OnFrame: s.onFrame,
	})
	if err != nil {
		return err
	}
	st, err := c.Join(ctx, room)
	if err != nil {
		_ = c.Close()
		return err
	}
	s.mu.Lock()
	s.c = c
	for _, p := range st.GetParticipants() {
		s.names[p.GetSessionId()] = p.GetDisplayName()
	}
	s.mu.Unlock()
	for _, t := range st.GetTracks() {
		s.addTrack(t)
	}
	s.setStatus("in %q with %d other(s)", room, len(st.GetParticipants()))
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-c.Events():
			if !ok {
				return fmt.Errorf("disconnected: %v", c.Err())
			}
			switch {
			case m.GetTrackAdded() != nil:
				s.addTrack(m.GetTrackAdded().GetTrack())
			case m.GetTrackRemoved() != nil:
				s.removeTrack(m.GetTrackRemoved().GetTrackId())
			case m.GetParticipantJoined() != nil:
				p := m.GetParticipantJoined().GetParticipant()
				s.mu.Lock()
				s.names[p.GetSessionId()] = p.GetDisplayName()
				s.mu.Unlock()
				s.setStatus("%s joined", p.GetDisplayName())
			case m.GetParticipantLeft() != nil:
				s.mu.Lock()
				name := s.names[m.GetParticipantLeft().GetSessionId()]
				s.mu.Unlock()
				s.setStatus("%s left", name)
			case m.GetSubscriptionState() != nil:
				st := m.GetSubscriptionState()
				s.mu.Lock()
				if r := s.remotes[st.GetTrackId()]; r != nil {
					r.state = st
				}
				s.mu.Unlock()
				s.invalidate()
			case m.GetError() != nil:
				s.setStatus("server: %s", m.GetError().GetMessage())
			}
		}
	}
}

func (s *session) client() *client.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

// ---- remote tracks ---------------------------------------------------------

func (s *session) addTrack(t *controlpb.Track) {
	id := t.GetTrackId()
	r := &remote{track: t, frames: make(chan client.RemoteFrame, 512)}
	dec, err := media.NewVideoDecoder(func(pic media.Picture, _ uint64) {
		s.mu.Lock()
		maxW := r.tileW
		s.mu.Unlock()
		img, err := media.ToRGBA(pic, max(maxW, 160))
		if err != nil {
			return
		}
		s.mu.Lock()
		r.img = img
		r.decoded++
		s.mu.Unlock()
		s.invalidate()
	})
	if err != nil {
		s.setStatus("no decoder: %v", err)
		return
	}
	r.dec = dec
	s.mu.Lock()
	if s.remotes[id] != nil {
		s.mu.Unlock()
		dec.Close()
		return
	}
	s.remotes[id] = r
	s.order = append(s.order, id)
	if s.pinned == 0 && t.GetKind() == controlpb.TrackKind_SCREEN {
		s.pinned = id // a screen share starts pinned
	}
	s.mu.Unlock()
	go s.decodeLoop(r)
	s.invalidate()
}

func (s *session) removeTrack(id uint32) {
	s.mu.Lock()
	r := s.remotes[id]
	delete(s.remotes, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if s.pinned == id {
		s.pinned = 0
	}
	s.mu.Unlock()
	if r != nil {
		close(r.frames)
	}
	s.invalidate()
}

func (s *session) onFrame(f client.RemoteFrame) {
	s.mu.Lock()
	r := s.remotes[f.TrackID]
	s.mu.Unlock()
	if r == nil {
		return
	}
	defer func() { _ = recover() }() // the track may go away under us
	select {
	case r.frames <- f:
	default: // decoder far behind: drop; it resyncs at the next group start
	}
}

// decodeLoop decodes one track in order. Groups arrive on their own
// streams and can overlap at a layer switch, so only frames of the newest
// group start are decoded.
func (s *session) decodeLoop(r *remote) {
	defer r.dec.Close()
	var layer uint32
	var group uint64
	synced := false
	for f := range r.frames {
		switch {
		case f.GroupStart:
			if synced && f.Layer == layer && f.Group <= group {
				continue // an older group finishing late
			}
			layer, group, synced = f.Layer, f.Group, true
		case !synced || f.Layer != layer || f.Group != group:
			continue
		}
		if err := r.dec.Decode(f.Payload, f.CaptureUS); err != nil {
			synced = false
			if c := s.client(); c != nil {
				c.ReportDecodeStall(r.track.GetTrackId())
			}
		}
	}
}

// togglePin pins a track big (or unpins it).
func (s *session) togglePin(id uint32) {
	s.mu.Lock()
	if s.pinned == id {
		s.pinned = 0
	} else {
		s.pinned = id
	}
	s.mu.Unlock()
	s.invalidate()
}

// tileView is what the UI draws for one remote tile.
type tileView struct {
	Tile
	img   *image.RGBA
	label string
}

// layout places tiles for a w×h (device pixel) area and makes the
// subscriptions follow: each visible tile asks for the layer its size
// needs; hidden tiles (and everything when the window is minimized) are
// unsubscribed.
func (s *session) layout(w, h int, visible bool) []tileView {
	s.mu.Lock()
	tiles := Layout(s.order, s.pinned, float64(w), float64(h), visible)
	c := s.c
	type change struct {
		id, layer uint32
		sub       bool
	}
	var changes []change
	views := make([]tileView, 0, len(tiles))
	for _, t := range tiles {
		r := s.remotes[t.TrackID]
		r.tileW = int(t.Rect.W)
		layer, ok := media.LayerForTile(r.track.GetLayers(), int(t.Rect.W), int(t.Rect.H), t.Visible)
		switch {
		case ok && (!r.subscribed || r.maxLayer != layer):
			r.subscribed, r.maxLayer = true, layer
			changes = append(changes, change{t.TrackID, layer, true})
		case !ok && r.subscribed:
			r.subscribed = false
			changes = append(changes, change{t.TrackID, 0, false})
		}
		views = append(views, tileView{Tile: t, img: r.img, label: s.labelLocked(r)})
	}
	s.lastTiles = tiles
	s.mu.Unlock()
	if c != nil {
		for _, ch := range changes {
			if ch.sub {
				_ = c.Subscribe(ch.id, ch.layer, 0)
			} else {
				_ = c.Unsubscribe(ch.id)
			}
		}
	}
	return views
}

func (s *session) labelLocked(r *remote) string {
	t := r.track
	name := s.names[t.GetSessionId()]
	if name == "" {
		name = fmt.Sprintf("session %d", t.GetSessionId())
	}
	kind := "camera"
	if t.GetKind() == controlpb.TrackKind_SCREEN {
		kind = "screen"
	}
	out := name + " · " + kind
	if st := r.state; st != nil {
		for _, l := range t.GetLayers() {
			if l.GetId() == st.GetLayer() && st.GetForwarding() {
				out += fmt.Sprintf(" · %dp", l.GetHeight())
			}
		}
		if st.GetPaused() {
			out += " · paused (bandwidth)"
		}
	}
	return out
}

// ---- local shares ----------------------------------------------------------

func (s *session) sharing() (camera, screen string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.camera != nil {
		camera = s.camera.name
	}
	if s.screen != nil {
		screen = s.screen.name
	}
	return
}

// StartCamera starts publishing a camera (cameraID "" = the default:
// built-in first) or, with pattern, a moving test pattern.
func (s *session) StartCamera(ctx context.Context, cameraID string, pattern bool) {
	name := "camera"
	start := func(push func(media.CapturedFrame)) (media.Source, error) {
		return media.StartCamera(media.CameraOptions{ID: cameraID, Width: 1280, Height: 720, FPS: 30}, push)
	}
	if pattern {
		name = "test pattern"
		start = func(push func(media.CapturedFrame)) (media.Source, error) {
			return media.NewPatternSource(1280, 720, 30, push)
		}
	}
	go s.startShare(ctx, controlpb.TrackKind_CAMERA, name, start)
}

// StartScreen shares a display (id) or window (windowID).
func (s *session) StartScreen(ctx context.Context, name string, displayID, windowID uint32) {
	go s.startShare(ctx, controlpb.TrackKind_SCREEN, name, func(push func(media.CapturedFrame)) (media.Source, error) {
		return media.StartScreen(media.ScreenOptions{DisplayID: displayID, WindowID: windowID, FPS: 15, Cursor: true}, push)
	})
}

func (s *session) StopCamera() { s.stopShare(&s.camera) }
func (s *session) StopScreen() { s.stopShare(&s.screen) }

func (s *session) stopShare(slot **localShare) {
	s.mu.Lock()
	ls := *slot
	*slot = nil
	if slot == &s.camera {
		s.preview = nil
	}
	s.mu.Unlock()
	if ls == nil {
		return
	}
	ls.src.Stop()
	ls.pub.Close()
	_ = ls.track.Unpublish()
	s.setStatus("stopped sharing %s", ls.name)
}

// startShare starts a source, learns its size from the first frame, and
// publishes a track with the matching layer ladder.
func (s *session) startShare(ctx context.Context, kind controlpb.TrackKind, name string, start func(func(media.CapturedFrame)) (media.Source, error)) {
	c := s.client()
	if c == nil {
		return
	}
	isCamera := kind == controlpb.TrackKind_CAMERA
	var mu sync.Mutex
	var pub *media.Publisher
	var lastPreview time.Time
	sized := make(chan [2]int, 1)
	push := func(f media.CapturedFrame) {
		if isCamera && time.Since(lastPreview) > 66*time.Millisecond { // self-preview at ~15 fps
			lastPreview = time.Now()
			if img, err := media.ToRGBA(f.Picture, 480); err == nil {
				s.mu.Lock()
				s.preview = img
				s.mu.Unlock()
				s.invalidate()
			}
		}
		mu.Lock()
		p := pub
		mu.Unlock()
		if p != nil {
			p.Push(f)
			return
		}
		w, h := f.Picture.Size()
		select {
		case sized <- [2]int{w, h}:
		default:
		}
	}
	src, err := start(push)
	if err != nil {
		var pe *media.PermissionError
		if errors.As(err, &pe) {
			s.setStatus("%s not shared — %s permission missing: %s", name, pe.What, pe.Help)
		} else {
			s.setStatus("%s not shared: %v", name, err)
		}
		return
	}
	var wh [2]int
	select {
	case wh = <-sized:
	case <-time.After(10 * time.Second):
		wh[0], wh[1] = src.Size()
	}
	if wh[0] == 0 || wh[1] == 0 {
		src.Stop()
		s.setStatus("%s not shared: it delivered no picture in 10 s (in use by another app?)", name)
		return
	}
	layers := media.CameraLayers(wh[0], wh[1], src.FPS())
	if !isCamera {
		layers = media.ScreenLayers(wh[0], wh[1])
	}
	p, track, err := media.PublishTrack(ctx, c, kind, layers, media.DefaultEncoder, media.PublisherOptions{
		OnError: func(l uint32, err error) { log.Printf("encoder layer %d: %v", l, err) },
	})
	if err != nil {
		src.Stop()
		s.setStatus("publishing %s failed: %v", name, err)
		return
	}
	mu.Lock()
	pub = p
	mu.Unlock()
	ls := &localShare{src: src, pub: p, track: track, name: name}
	s.mu.Lock()
	slot := &s.screen
	if isCamera {
		slot = &s.camera
	}
	old := *slot
	*slot = ls
	s.mu.Unlock()
	if old != nil {
		old.src.Stop()
		old.pub.Close()
		_ = old.track.Unpublish()
	}
	var desc []string
	for _, l := range layers {
		desc = append(desc, fmt.Sprintf("%dp@%d", l.GetHeight(), l.GetFps()))
	}
	s.setStatus("sharing %s %dx%d (%s)", name, wh[0], wh[1], strings.Join(desc, ", "))
}

// Close leaves the call.
func (s *session) Close() {
	s.StopCamera()
	s.StopScreen()
	if c := s.client(); c != nil {
		_ = c.Leave()
		_ = c.Close()
	}
}
