//go:build darwin

// macviewer is a small native macOS client for quicvideo: join a room,
// publish your camera and/or screen (or a test pattern), and watch everyone
// else's tracks in a grid. Click a tile to pin it big; click it again to
// unpin. Each tile subscribes to the layer that fits its size, and tiles
// that aren't shown (or a minimized window) are unsubscribed.
//
//	go run ./examples/macviewer -insecure -token alice -camera -screen
//	go run ./examples/macviewer -list
package main

/*
#cgo CFLAGS: -fobjc-arc -Wno-unused-command-line-argument -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework AppKit -framework QuartzCore -framework CoreVideo -Wl,-sectcreate,__TEXT,__info_plist,${SRCDIR}/Info.plist
#include <stdlib.h>
#include "ui_darwin.h"
*/
import "C"

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/client/media"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

func init() { runtime.LockOSThread() } // AppKit runs on the main thread

var (
	addr      = flag.String("addr", "localhost:7710", "quicvideod address")
	token     = flag.String("token", "", "token (with AUTH_INSECURE: any user id)")
	room      = flag.String("room", "call", "room")
	insecure  = flag.Bool("insecure", false, "skip TLS verification (self-signed dev server)")
	camera    = flag.Bool("camera", false, "publish the camera")
	cameraID  = flag.String("camera-id", "", "camera unique id (see -list); default camera if empty")
	screen    = flag.Bool("screen", false, "publish the screen")
	displayID = flag.Uint("display", 0, "display id to share (see -list); main display if 0")
	windowID  = flag.Uint("window", 0, "window id to share instead of a display")
	pattern   = flag.Bool("pattern", false, "publish a moving test pattern as a camera (needs no permissions)")
	list      = flag.Bool("list", false, "list displays, windows, cameras and permissions, then exit")
	snapshot  = flag.String("snapshot", "", "write a PNG of the tiles to this path after -snapshot-after")
	snapAfter = flag.Duration("snapshot-after", 8*time.Second, "when to write -snapshot")
	quitAfter = flag.Duration("quit-after", 0, "quit after this long (0 = run until the window closes)")
)

func main() {
	flag.Parse()
	if *list {
		printDevices()
		return
	}
	a := theApp
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := a.run(ctx); err != nil {
			log.Printf("error: %v", err)
			C.qv_ui_quit()
		}
	}()
	if *quitAfter > 0 {
		time.AfterFunc(*quitAfter, func() { C.qv_ui_quit() })
	}
	title := C.CString("quicvideo — " + *room)
	C.qv_ui_run(title, 1280, 800) // returns when the app quits
	C.free(unsafe.Pointer(title))
	cancel()
	a.close()
}

func printDevices() {
	fmt.Printf("Screen Recording permission: %s\n", media.ScreenPermission())
	fmt.Printf("Camera permission: %s\n", media.CameraPermission())
	if ds, ws, err := media.ListScreens(); err != nil {
		fmt.Printf("screens: %v\n", err)
	} else {
		for _, d := range ds {
			fmt.Printf("display %d: %dx%d main=%v\n", d.ID, d.Width, d.Height, d.Main)
		}
		for _, w := range ws {
			fmt.Printf("window %d: %q (%s) %dx%d\n", w.ID, w.Title, w.App, w.Width, w.Height)
		}
	}
	cams, err := media.ListCameras()
	if err != nil {
		fmt.Printf("cameras: %v\n", err)
	}
	for _, c := range cams {
		sort.Slice(c.Formats, func(i, j int) bool {
			return c.Formats[i].Width*c.Formats[i].Height < c.Formats[j].Width*c.Formats[j].Height
		})
		var fs []string
		for _, f := range c.Formats {
			fs = append(fs, fmt.Sprintf("%dx%d@%d", f.Width, f.Height, f.MaxFPS))
		}
		fmt.Printf("camera %q: %s [%s]\n", c.ID, c.Name, strings.Join(fs, " "))
	}
}

// ---- app -------------------------------------------------------------------

type remote struct {
	track      *controlpb.Track
	frames     chan client.RemoteFrame
	dec        *media.Decoder
	subscribed bool
	maxLayer   uint32
	state      *controlpb.SubscriptionState
	last       *media.PixelBuffer // latest decoded picture (retained), for snapshots
	decoded    int
}

type app struct {
	c *client.Client

	mu       sync.Mutex
	remotes  map[uint32]*remote
	order    []uint32
	names    map[uint32]string // session id → name
	pinned   uint32
	w, h     float64
	scale    float64
	visible  bool
	sources  []media.Source
	pubs     []*media.Publisher
	snapDone bool
}

func newApp() *app {
	return &app{remotes: map[uint32]*remote{}, names: map[uint32]string{}, scale: 2, visible: true}
}

func (a *app) run(ctx context.Context) error {
	c, err := client.Dial(ctx, *addr, client.Options{
		Token: *token, ClientVersion: "macviewer/dev", TLS: &tls.Config{InsecureSkipVerify: *insecure},
		OnFrame: a.onFrame,
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.c = c
	a.mu.Unlock()
	st, err := c.Join(ctx, *room)
	if err != nil {
		return err
	}
	a.mu.Lock()
	for _, p := range st.GetParticipants() {
		a.names[p.GetSessionId()] = p.GetDisplayName()
	}
	a.mu.Unlock()
	for _, t := range st.GetTracks() {
		a.addTrack(t)
	}
	log.Printf("joined %q as session %d (%d tracks here)", *room, c.SessionID(), len(st.GetTracks()))

	if *pattern {
		a.publish(ctx, controlpb.TrackKind_CAMERA, func(push func(media.CapturedFrame)) (media.Source, error) {
			return media.NewPatternSource(1280, 720, 30, push)
		})
	}
	if *camera {
		a.publish(ctx, controlpb.TrackKind_CAMERA, func(push func(media.CapturedFrame)) (media.Source, error) {
			return media.StartCamera(media.CameraOptions{ID: *cameraID, Width: 1280, Height: 720, FPS: 30}, push)
		})
	}
	if *screen {
		a.publish(ctx, controlpb.TrackKind_SCREEN, func(push func(media.CapturedFrame)) (media.Source, error) {
			return media.StartScreen(media.ScreenOptions{DisplayID: uint32(*displayID), WindowID: uint32(*windowID), FPS: 15, Cursor: true}, push)
		})
	}
	if *snapshot != "" {
		time.AfterFunc(*snapAfter, a.writeSnapshot)
	}

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
				a.addTrack(m.GetTrackAdded().GetTrack())
			case m.GetTrackRemoved() != nil:
				a.removeTrack(m.GetTrackRemoved().GetTrackId())
			case m.GetParticipantJoined() != nil:
				p := m.GetParticipantJoined().GetParticipant()
				a.mu.Lock()
				a.names[p.GetSessionId()] = p.GetDisplayName()
				a.mu.Unlock()
				a.relayout()
			case m.GetSubscriptionState() != nil:
				s := m.GetSubscriptionState()
				a.mu.Lock()
				if r := a.remotes[s.GetTrackId()]; r != nil {
					r.state = s
				}
				a.mu.Unlock()
				a.relayout()
			case m.GetError() != nil:
				log.Printf("server: %s %s", m.GetError().GetCode(), m.GetError().GetMessage())
			}
		}
	}
}

// publish starts a source, waits for its first frame to learn the size,
// then publishes a track with the matching layer ladder.
func (a *app) publish(ctx context.Context, kind controlpb.TrackKind, start func(func(media.CapturedFrame)) (media.Source, error)) {
	var mu sync.Mutex
	var pub *media.Publisher
	sized := make(chan [2]int, 1)
	push := func(f media.CapturedFrame) {
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
			log.Printf("%s not shared: %v", strings.ToLower(kind.String()), pe)
		} else {
			log.Printf("%s not shared: %v", strings.ToLower(kind.String()), err)
		}
		return
	}
	var wh [2]int
	select {
	case wh = <-sized:
	case <-time.After(5 * time.Second):
		// A static screen sends its first frame right away; a camera within
		// a second. Fall back to the source's own idea of its size.
		wh[0], wh[1] = src.Size()
	}
	layers := media.CameraLayers(wh[0], wh[1], src.FPS())
	if kind == controlpb.TrackKind_SCREEN {
		layers = media.ScreenLayers(wh[0], wh[1])
	}
	p, track, err := media.PublishTrack(ctx, a.c, kind, layers, media.DefaultEncoder, media.PublisherOptions{
		OnError: func(l uint32, err error) { log.Printf("encoder layer %d: %v", l, err) },
	})
	if err != nil {
		src.Stop()
		log.Printf("publish %s: %v", kind, err)
		return
	}
	mu.Lock()
	pub = p
	mu.Unlock()
	a.mu.Lock()
	a.sources = append(a.sources, src)
	a.pubs = append(a.pubs, p)
	a.mu.Unlock()
	var desc []string
	for _, l := range layers {
		desc = append(desc, fmt.Sprintf("%dx%d@%d %dk", l.GetWidth(), l.GetHeight(), l.GetFps(), l.GetBitrate()/1000))
	}
	log.Printf("publishing %s %dx%d as track %d: %s", kind, wh[0], wh[1], track.ID(), strings.Join(desc, ", "))
}

func (a *app) addTrack(t *controlpb.Track) {
	r := &remote{track: t, frames: make(chan client.RemoteFrame, 512)}
	id := t.GetTrackId()
	dec, err := media.NewDecoder(func(pb *media.PixelBuffer, _ uint64) {
		C.qv_ui_set_frame(C.uint32_t(id), pb.Ref())
		pb.Retain()
		a.mu.Lock()
		if r.last != nil {
			r.last.Release()
		}
		r.last = pb
		r.decoded++
		a.mu.Unlock()
	})
	if err != nil {
		log.Printf("decoder: %v", err)
		return
	}
	r.dec = dec
	a.mu.Lock()
	if a.remotes[id] != nil {
		a.mu.Unlock()
		dec.Close()
		return
	}
	a.remotes[id] = r
	a.order = append(a.order, id)
	if a.pinned == 0 && t.GetKind() == controlpb.TrackKind_SCREEN {
		a.pinned = id // a screen share starts pinned
	}
	a.mu.Unlock()
	go a.decodeLoop(r)
	a.relayout()
}

func (a *app) removeTrack(id uint32) {
	a.mu.Lock()
	r := a.remotes[id]
	delete(a.remotes, id)
	for i, x := range a.order {
		if x == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	if a.pinned == id {
		a.pinned = 0
	}
	a.mu.Unlock()
	if r != nil {
		close(r.frames)
	}
	a.relayout()
}

func (a *app) onFrame(f client.RemoteFrame) {
	a.mu.Lock()
	r := a.remotes[f.TrackID]
	a.mu.Unlock()
	if r == nil {
		return
	}
	defer func() { _ = recover() }() // the channel may close under us when the track goes
	select {
	case r.frames <- f:
	default: // decoder far behind: drop; it resyncs at the next group start
	}
}

// decodeLoop decodes one track's frames in order. Groups arrive on their
// own streams and can overlap at a switch, so only frames of the most
// recent group start are decoded.
func (a *app) decodeLoop(r *remote) {
	defer func() {
		r.dec.Close()
		a.mu.Lock()
		if r.last != nil {
			r.last.Release()
			r.last = nil
		}
		a.mu.Unlock()
	}()
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
			a.c.ReportDecodeStall(r.track.GetTrackId())
		}
	}
}

// ---- layout & subscriptions ------------------------------------------------

func (a *app) label(r *remote) string {
	t := r.track
	name := a.names[t.GetSessionId()]
	if name == "" {
		name = fmt.Sprintf("session %d", t.GetSessionId())
	}
	kind := "camera"
	if t.GetKind() == controlpb.TrackKind_SCREEN {
		kind = "screen"
	}
	s := name + " · " + kind
	if st := r.state; st != nil {
		for _, l := range t.GetLayers() {
			if l.GetId() == st.GetLayer() {
				s += fmt.Sprintf(" · %dp", l.GetHeight())
			}
		}
		if st.GetPaused() {
			s += " · paused (bandwidth)"
		}
	}
	return s
}

func (a *app) relayout() {
	a.mu.Lock()
	c := a.c
	tiles := Layout(a.order, a.pinned, a.w, a.h, a.visible)
	type change struct {
		id, layer uint32
		sub       bool
	}
	var changes []change
	n := len(tiles)
	ids := make([]C.uint32_t, n)
	rects := make([]C.double, 4*n)
	vis := make([]C.int, n)
	labels := make([]*C.char, n)
	for i, t := range tiles {
		r := a.remotes[t.TrackID]
		ids[i] = C.uint32_t(t.TrackID)
		rects[4*i], rects[4*i+1], rects[4*i+2], rects[4*i+3] = C.double(t.Rect.X), C.double(t.Rect.Y), C.double(t.Rect.W), C.double(t.Rect.H)
		if t.Visible {
			vis[i] = 1
		}
		labels[i] = C.CString(a.label(r))
		layer, ok := media.LayerForTile(r.track.GetLayers(), int(t.Rect.W*a.scale), int(t.Rect.H*a.scale), t.Visible)
		switch {
		case ok && (!r.subscribed || r.maxLayer != layer):
			r.subscribed, r.maxLayer = true, layer
			changes = append(changes, change{t.TrackID, layer, true})
		case !ok && r.subscribed:
			r.subscribed = false
			changes = append(changes, change{t.TrackID, 0, false})
		}
	}
	a.mu.Unlock()
	if n > 0 {
		C.qv_ui_set_tiles(C.int(n), &ids[0], &rects[0], &labels[0], &vis[0])
	} else {
		C.qv_ui_set_tiles(0, nil, nil, nil, nil)
	}
	for _, l := range labels {
		C.free(unsafe.Pointer(l))
	}
	if c == nil {
		return
	}
	for _, ch := range changes {
		if ch.sub {
			_ = c.Subscribe(ch.id, ch.layer, 0)
		} else {
			_ = c.Unsubscribe(ch.id)
		}
	}
}

//export qvGoUIResize
func qvGoUIResize(w, h, scale C.double) {
	theApp.mu.Lock()
	theApp.w, theApp.h, theApp.scale = float64(w), float64(h), float64(scale)
	theApp.mu.Unlock()
	go theApp.relayout()
}

//export qvGoUIClick
func qvGoUIClick(id C.uint32_t) {
	theApp.mu.Lock()
	if theApp.pinned == uint32(id) {
		theApp.pinned = 0
	} else {
		theApp.pinned = uint32(id)
	}
	theApp.mu.Unlock()
	go theApp.relayout()
}

//export qvGoUIVisible
func qvGoUIVisible(v C.int) {
	theApp.mu.Lock()
	changed := theApp.visible != (v != 0)
	theApp.visible = v != 0
	theApp.mu.Unlock()
	if changed {
		go theApp.relayout()
	}
}

//export qvGoUIClosed
func qvGoUIClosed() {}

// theApp is the one app; the UI callbacks reach it here.
var theApp = newApp()

func (a *app) close() {
	a.mu.Lock()
	srcs, pubs, c := a.sources, a.pubs, a.c
	a.mu.Unlock()
	for _, s := range srcs {
		s.Stop()
	}
	for _, p := range pubs {
		p.Close()
	}
	if c != nil {
		_ = c.Close()
	}
}

// writeSnapshot composites the visible tiles' latest pictures (at 1x) into
// a PNG, and, if this process may record the screen, also saves a real
// window capture next to it.
func (a *app) writeSnapshot() {
	a.mu.Lock()
	tiles := Layout(a.order, a.pinned, a.w, a.h, true)
	img := image.NewRGBA(image.Rect(0, 0, int(a.w), int(a.h)))
	draw.Draw(img, img.Bounds(), image.NewUniform(colorBG), image.Point{}, draw.Src)
	var info []string
	for _, t := range tiles {
		r := a.remotes[t.TrackID]
		if !t.Visible || r == nil {
			continue
		}
		rect := image.Rect(int(t.Rect.X), int(t.Rect.Y), int(t.Rect.X+t.Rect.W), int(t.Rect.Y+t.Rect.H))
		draw.Draw(img, rect, image.NewUniform(colorTile), image.Point{}, draw.Src)
		if r.last != nil {
			if src, err := r.last.RGBA(); err == nil {
				drawFit(img, rect, src)
			}
		}
		info = append(info, fmt.Sprintf("%s (%d frames decoded)", a.label(r), r.decoded))
	}
	a.mu.Unlock()
	f, err := os.Create(*snapshot)
	if err != nil {
		log.Printf("snapshot: %v", err)
		return
	}
	_ = png.Encode(f, img)
	f.Close()
	log.Printf("snapshot (tile composite) written to %s: %s", *snapshot, strings.Join(info, "; "))
	if media.ScreenPermission() == media.PermissionGranted {
		win := strings.TrimSuffix(*snapshot, ".png") + "-window.png"
		wid := int(C.qv_ui_window_id())
		if out, err := exec.Command("screencapture", "-x", "-o", fmt.Sprintf("-l%d", wid), win).CombinedOutput(); err == nil {
			log.Printf("window screenshot written to %s", win)
		} else {
			log.Printf("window screenshot failed: %v %s", err, out)
		}
	} else {
		log.Printf("no window screenshot: this process lacks the Screen Recording permission")
	}
}
