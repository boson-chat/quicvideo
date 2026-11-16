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
	"sync/atomic"
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
	camera    shareSlot
	screen    shareSlot
	preview   *image.RGBA // our camera, small
	status    string
	lastTiles []Tile
	scale     float64
	closed    bool

	closeOnce sync.Once
	// publish publishes a local track (tests swap in a fake).
	publish func(ctx context.Context, kind controlpb.TrackKind, layers []*controlpb.Layer) (publication, error)
	// stallAfter: a camera that delivers no picture for this long is
	// treated as stopped (screens only send frames on change).
	stallAfter time.Duration
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

// publication is a published local track: encoders plus the track.
type publication interface {
	Push(media.CapturedFrame)
	// Close stops the encoders and unpublishes the track.
	Close()
}

// shareSlot is the camera or the screen share. gen changes on every start
// and stop, so a start that finishes after a newer start or a stop knows
// it's stale and releases its device instead of installing itself.
type shareSlot struct {
	gen     uint64
	pending string        // name while a start is in progress
	cancel  chan struct{} // closed to abandon the start in progress
	cur     *localShare
}

type localShare struct {
	src  media.Source
	pub  publication
	name string
	once sync.Once
	done chan struct{}
}

// stop releases the capture device first (the camera light goes off even
// if an encoder is slow), then stops the encoders and unpublishes.
func (ls *localShare) stop() {
	ls.once.Do(func() {
		close(ls.done)
		ls.src.Stop()
		ls.pub.Close()
	})
}

type trackPublication struct {
	pub   *media.Publisher
	track *client.LocalTrack
}

func (t *trackPublication) Push(f media.CapturedFrame) { t.pub.Push(f) }
func (t *trackPublication) Close() {
	t.pub.Close()
	_ = t.track.Unpublish()
}

func newSession(invalidate func()) *session {
	s := &session{remotes: map[uint32]*remote{}, names: map[uint32]string{}, invalidate: invalidate, scale: 1,
		stallAfter: 5 * time.Second}
	s.publish = s.publishTrack
	return s
}

func (s *session) publishTrack(ctx context.Context, kind controlpb.TrackKind, layers []*controlpb.Layer) (publication, error) {
	c := s.client()
	if c == nil {
		return nil, errors.New("not connected")
	}
	p, track, err := media.PublishTrack(ctx, c, kind, layers, media.DefaultEncoder, media.PublisherOptions{
		OnError: func(l uint32, err error) { log.Printf("encoder layer %d: %v", l, err) },
	})
	if err != nil {
		return nil, err
	}
	return &trackPublication{pub: p, track: track}, nil
}

func (s *session) setStatus(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.status = msg
	s.mu.Unlock()
	log.Print(msg)
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

func (s *session) slot(kind controlpb.TrackKind) *shareSlot {
	if kind == controlpb.TrackKind_CAMERA {
		return &s.camera
	}
	return &s.screen
}

// sharing names the camera/screen share, including one still starting
// (the UI shows "off" for it, and clicking cancels the start).
func (s *session) sharing() (camera, screen string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := func(sl *shareSlot) string {
		if sl.cur != nil {
			return sl.cur.name
		}
		return sl.pending
	}
	return name(&s.camera), name(&s.screen)
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

func (s *session) StopCamera() { s.stopShare(controlpb.TrackKind_CAMERA) }
func (s *session) StopScreen() { s.stopShare(controlpb.TrackKind_SCREEN) }

// stopShare stops the share, or abandons one that is still starting.
func (s *session) stopShare(kind controlpb.TrackKind) {
	s.mu.Lock()
	sl := s.slot(kind)
	sl.gen++
	if sl.cancel != nil {
		close(sl.cancel)
		sl.cancel = nil
	}
	pending := sl.pending
	sl.pending = ""
	ls := sl.cur
	sl.cur = nil
	if kind == controlpb.TrackKind_CAMERA {
		s.preview = nil
	}
	s.mu.Unlock()
	s.invalidate()
	switch {
	case ls != nil:
		ls.stop()
		s.setStatus("stopped sharing %s", ls.name)
	case pending != "":
		s.setStatus("%s: start cancelled", pending)
	}
}

// shareEnded turns a share off that ended on its own (device error or
// unplugged, capture interrupted, no pictures): it is unpublished so the
// others don't keep a frozen tile, and the UI shows it off.
func (s *session) shareEnded(kind controlpb.TrackKind, ls *localShare, why error) {
	s.mu.Lock()
	sl := s.slot(kind)
	if sl.cur != ls {
		s.mu.Unlock()
		return // already stopped or replaced
	}
	sl.cur = nil // a start in progress (Change screen) carries on
	if kind == controlpb.TrackKind_CAMERA {
		s.preview = nil
	}
	s.mu.Unlock()
	ls.stop()
	s.setStatus("%s stopped: %v", ls.name, why)
}

// watch ends a share whose source reports an end (media.Ender) or, for a
// camera, stops delivering pictures.
func (s *session) watch(kind controlpb.TrackKind, ls *localShare, lastFrame *atomic.Int64) {
	var ended <-chan error
	if e, ok := ls.src.(media.Ender); ok {
		ended = e.Ended()
	}
	var tick <-chan time.Time
	if kind == controlpb.TrackKind_CAMERA && s.stallAfter > 0 {
		t := time.NewTicker(max(s.stallAfter/5, 10*time.Millisecond))
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ls.done:
			return
		case err := <-ended:
			s.shareEnded(kind, ls, err)
			return
		case <-tick:
			if since := time.Since(time.Unix(0, lastFrame.Load())); since > s.stallAfter {
				s.shareEnded(kind, ls, fmt.Errorf("no picture for %v", s.stallAfter))
				return
			}
		}
	}
}

// startShare starts a source, learns its size from the first frame, and
// publishes a track with the matching layer ladder. A stop (or a newer
// start, or Close) while it runs wins: the source is released and nothing
// is published.
func (s *session) startShare(ctx context.Context, kind controlpb.TrackKind, name string, start func(func(media.CapturedFrame)) (media.Source, error)) {
	isCamera := kind == controlpb.TrackKind_CAMERA
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	sl := s.slot(kind)
	sl.gen++
	gen := sl.gen
	if sl.cancel != nil {
		close(sl.cancel) // a start still in progress is superseded
	}
	cancel := make(chan struct{})
	sl.cancel = cancel
	sl.pending = name
	s.mu.Unlock()
	s.invalidate()

	live := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.closed && sl.gen == gen
	}
	fail := func(format string, args ...any) {
		s.mu.Lock()
		if sl.gen == gen {
			sl.pending = ""
			sl.cancel = nil
		}
		s.mu.Unlock()
		s.setStatus(format, args...)
	}

	var mu sync.Mutex
	var pub publication
	var lastPreview time.Time
	var lastFrame atomic.Int64
	sized := make(chan [2]int, 1)
	push := func(f media.CapturedFrame) {
		lastFrame.Store(time.Now().UnixNano())
		if isCamera && time.Since(lastPreview) > 66*time.Millisecond { // self-preview at ~15 fps
			lastPreview = time.Now()
			if img, err := media.ToRGBA(f.Picture, 480); err == nil {
				s.mu.Lock()
				if sl.gen == gen && !s.closed {
					s.preview = img
				}
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
			fail("%s not shared — %s permission missing: %s", name, pe.What, pe.Help)
		} else {
			fail("%s not shared: %v", name, err)
		}
		return
	}
	var wh [2]int
	select {
	case wh = <-sized:
	case <-time.After(10 * time.Second):
		wh[0], wh[1] = src.Size()
	case <-cancel: // stopped (or restarted) while starting
		src.Stop()
		return
	}
	if !live() {
		src.Stop()
		return
	}
	if wh[0] == 0 || wh[1] == 0 {
		src.Stop()
		fail("%s not shared: it delivered no picture in 10 s (in use by another app?)", name)
		return
	}
	layers := media.CameraLayers(wh[0], wh[1], src.FPS())
	if !isCamera {
		layers = media.ScreenLayers(wh[0], wh[1])
	}
	p, err := s.publish(ctx, kind, layers)
	if err != nil {
		src.Stop()
		fail("publishing %s failed: %v", name, err)
		return
	}
	ls := &localShare{src: src, pub: p, name: name, done: make(chan struct{})}
	s.mu.Lock()
	if s.closed || sl.gen != gen {
		s.mu.Unlock()
		ls.stop()
		return
	}
	old := sl.cur
	sl.cur, sl.pending, sl.cancel = ls, "", nil
	s.mu.Unlock()
	mu.Lock()
	pub = p
	mu.Unlock()
	if old != nil {
		old.stop()
	}
	go s.watch(kind, ls, &lastFrame)
	var desc []string
	for _, l := range layers {
		desc = append(desc, fmt.Sprintf("%dp@%d", l.GetHeight(), l.GetFps()))
	}
	s.setStatus("sharing %s %dx%d (%s)", name, wh[0], wh[1], strings.Join(desc, ", "))
}

// Close leaves the call: every share is stopped (devices released, tracks
// unpublished) and no new one can start. Safe to call more than once and
// from any goroutine.
func (s *session) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.StopCamera()
		s.StopScreen()
		if c := s.client(); c != nil {
			_ = c.Leave()
			_ = c.Close()
		}
	})
}
