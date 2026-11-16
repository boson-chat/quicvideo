// viewer is the quicvideo desktop test client (macOS now; Windows and
// Linux as their capture/codec backends land): join a room, share your
// camera and/or screen, and watch everyone else's tracks.
//
//   - Top bar: camera on/off, share screen (pick a display or window), stop
//     sharing, leave.
//
//   - Tiles: a grid, or a pinned big tile plus a strip (click a tile to pin
//     or unpin it; a screen share starts pinned). Each tile subscribes to
//     the layer that fits its size in device pixels; tiles that aren't
//     shown, and everything while the window is minimized, unsubscribe.
//
//   - Your own camera is previewed in the bottom-right corner.
//
//     go run ./examples/viewer -insecure -token alice -camera -screen
//     go run ./examples/viewer -list
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/boson-chat/quicvideo/client/media"
)

var (
	addr      = flag.String("addr", "localhost:7710", "quicvideod address")
	token     = flag.String("token", "", "token (with AUTH_INSECURE: any user id)")
	room      = flag.String("room", "call", "room")
	insecure  = flag.Bool("insecure", false, "skip TLS verification (self-signed dev server)")
	camera    = flag.Bool("camera", false, "start with the camera on")
	cameraID  = flag.String("camera-id", "", "camera id (see -list); default: built-in, then physical, then others")
	screen    = flag.Bool("screen", false, "start sharing the main display")
	pattern   = flag.Bool("pattern", false, "use a moving test pattern as the camera (needs no permissions)")
	list      = flag.Bool("list", false, "list displays, windows, cameras and permissions, then exit")
	snapshot  = flag.String("snapshot", "", "write a PNG of the window to this path after -snapshot-after")
	snapAfter = flag.Duration("snapshot-after", 8*time.Second, "when to write -snapshot")
	quitAfter = flag.Duration("quit-after", 0, "quit after this long (0 = until the window closes)")
)

func main() {
	flag.Parse()
	if *list {
		printDevices()
		return
	}
	go func() {
		w := new(app.Window)
		w.Option(app.Title("quicvideo — "+*room), app.Size(unit.Dp(1280), unit.Dp(800)))
		if err := run(w); err != nil {
			log.Print(err)
		}
		os.Exit(0)
	}()
	app.Main()
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
	def, _ := media.DefaultCamera(cams)
	for _, c := range cams {
		sort.Slice(c.Formats, func(i, j int) bool {
			return c.Formats[i].Width*c.Formats[i].Height < c.Formats[j].Width*c.Formats[j].Height
		})
		var fs []string
		for _, f := range c.Formats {
			fs = append(fs, fmt.Sprintf("%dx%d@%d", f.Width, f.Height, f.MaxFPS))
		}
		mark := ""
		if c.ID == def.ID {
			mark = " (default)"
		}
		fmt.Printf("camera %q: %s, %s%s [%s]\n", c.ID, c.Name, c.Kind, mark, strings.Join(fs, " "))
	}
}

// quitter returns the one way out: leave the call (shares stopped, devices
// released, tracks unpublished), then exit. If leaving hangs, exit anyway
// after 5 s; the OS releases devices of an exited process.
func quitter(s *session) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			done := make(chan struct{})
			go func() {
				s.Close()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				log.Print("leaving took over 5 s; exiting anyway")
			}
			os.Exit(0)
		})
	}
}

// ui holds the widgets' state.
type ui struct {
	th                                *material.Theme
	camBtn, screenBtn, stopBtn, leave widget.Clickable
	picking                           bool
	sources                           []source
	srcBtns                           []widget.Clickable
	tileBtns                          map[uint32]*widget.Clickable
	visible                           bool
	snapDue                           time.Time
	quit                              func()
}

type source struct {
	name            string
	display, window uint32
}

func run(w *app.Window) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSession(w.Invalidate)
	go func() {
		if err := s.run(ctx, *addr, *token, *room, *insecure); err != nil {
			s.setStatus("error: %v", err)
		}
	}()
	go func() { // initial shares once connected
		for s.client() == nil {
			time.Sleep(50 * time.Millisecond)
		}
		if *camera || *pattern {
			s.StartCamera(ctx, *cameraID, *pattern)
		}
		if *screen {
			s.StartScreen(ctx, "main display", 0, 0)
		}
	}()
	// Quitting doesn't go through closing the window: on macOS AppKit
	// ignores performClose: while ScreenCaptureKit shares the display, so
	// -quit-after and Leave never got a DestroyEvent and the viewer (and its
	// camera) kept running.
	quit := quitter(s)
	if *quitAfter > 0 {
		time.AfterFunc(*quitAfter, quit)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		quit()
	}()
	u := &ui{th: material.NewTheme(), tileBtns: map[uint32]*widget.Clickable{}, visible: true, quit: quit}
	u.th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	u.th.Palette = material.Palette{Bg: color.NRGBA{15, 17, 21, 255}, Fg: color.NRGBA{230, 232, 235, 255},
		ContrastBg: color.NRGBA{58, 110, 220, 255}, ContrastFg: color.NRGBA{255, 255, 255, 255}}
	if *snapshot != "" {
		u.snapDue = time.Now().Add(*snapAfter)
		time.AfterFunc(*snapAfter, w.Invalidate)
	}
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			s.Close()
			return e.Err
		case app.ConfigEvent:
			vis := e.Config.Mode != app.Minimized
			if vis != u.visible {
				u.visible = vis
				w.Invalidate()
			}
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			views := u.frame(gtx, ctx, s, w)
			e.Frame(gtx.Ops)
			if !u.snapDue.IsZero() && time.Now().After(u.snapDue) {
				u.snapDue = time.Time{}
				writeSnapshot(*snapshot, e.Size, views, s)
				go windowScreenshot(*snapshot)
			}
		}
	}
}

var (
	bgColor   = color.NRGBA{15, 17, 21, 255}
	tileColor = color.NRGBA{31, 33, 37, 255}
	barColor  = color.NRGBA{24, 26, 30, 255}
)

func (u *ui) frame(gtx layout.Context, ctx context.Context, s *session, w *app.Window) []tileView {
	paint.Fill(gtx.Ops, bgColor)
	camName, screenName := s.sharing()

	// Controls.
	if u.camBtn.Clicked(gtx) {
		if camName != "" {
			go s.StopCamera()
		} else {
			s.StartCamera(ctx, *cameraID, *pattern)
		}
	}
	if u.screenBtn.Clicked(gtx) {
		u.picking = !u.picking
		if u.picking {
			u.loadSources()
		}
	}
	if u.stopBtn.Clicked(gtx) {
		go s.StopScreen()
	}
	if u.leave.Clicked(gtx) {
		go u.quit()
	}
	for i := range u.srcBtns {
		if u.srcBtns[i].Clicked(gtx) {
			src := u.sources[i]
			u.picking = false
			s.StartScreen(ctx, src.name, src.display, src.window)
		}
	}

	var views []tileView
	layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.bar(gtx, s, camName, screenName)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			size := gtx.Constraints.Max
			views = s.layout(size.X, size.Y, u.visible)
			for _, v := range views {
				u.tile(gtx, s, v)
			}
			if u.picking {
				u.picker(gtx)
			}
			u.selfPreview(gtx, s, size)
			return layout.Dimensions{Size: size}
		}),
	)
	return views
}

func (u *ui) bar(gtx layout.Context, s *session, camName, screenName string) layout.Dimensions {
	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			paint.FillShape(gtx.Ops, barColor, clip.Rect{Max: gtx.Constraints.Min}.Op())
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				camLabel := "Camera on"
				if camName != "" {
					camLabel = "Camera off"
				}
				shareLabel := "Share screen"
				if screenName != "" {
					shareLabel = "Change screen"
				}
				items := []layout.FlexChild{
					layout.Rigid(material.Button(u.th, &u.camBtn, camLabel).Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(material.Button(u.th, &u.screenBtn, shareLabel).Layout),
				}
				if screenName != "" {
					items = append(items, layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(material.Button(u.th, &u.stopBtn, "Stop sharing").Layout))
				}
				s.mu.Lock()
				status := s.status
				s.mu.Unlock()
				items = append(items,
					layout.Rigid(layout.Spacer{Width: unit.Dp(16)}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						l := material.Body2(u.th, status)
						l.MaxLines = 2
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(u.th, &u.leave, "Leave")
						b.Background = color.NRGBA{190, 50, 50, 255}
						return b.Layout(gtx)
					}),
				)
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx, items...)
			})
		})
}

// tile draws one remote track at its layout rect.
func (u *ui) tile(gtx layout.Context, s *session, v tileView) {
	if !v.Visible {
		return
	}
	r := image.Rect(int(v.Rect.X), int(v.Rect.Y), int(v.Rect.X+v.Rect.W), int(v.Rect.Y+v.Rect.H))
	defer op.Offset(r.Min).Push(gtx.Ops).Pop()
	gtx.Constraints = layout.Exact(r.Size())
	defer clip.UniformRRect(image.Rectangle{Max: r.Size()}, gtx.Dp(8)).Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, tileColor)
	btn := u.tileBtns[v.TrackID]
	if btn == nil {
		btn = new(widget.Clickable)
		u.tileBtns[v.TrackID] = btn
	}
	if btn.Clicked(gtx) {
		s.togglePin(v.TrackID)
	}
	btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if v.img != nil {
			img := widget.Image{Src: paint.NewImageOp(v.img), Fit: widget.Contain, Position: layout.Center, Scale: 1 / gtx.Metric.PxPerDp}
			layout.Center.Layout(gtx, img.Layout)
		}
		layout.NW.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, label(u.th, v.label))
		})
		return layout.Dimensions{Size: gtx.Constraints.Max}
	})
}

func label(th *material.Theme, s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Background{}.Layout(gtx,
			func(gtx layout.Context) layout.Dimensions {
				rr := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(4))
				paint.FillShape(gtx.Ops, color.NRGBA{0, 0, 0, 150}, rr.Op(gtx.Ops))
				return layout.Dimensions{Size: gtx.Constraints.Min}
			},
			func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: unit.Dp(6), Right: unit.Dp(6), Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx,
					material.Caption(th, s).Layout)
			})
	}
}

// selfPreview shows our own camera small in the bottom-right corner.
func (u *ui) selfPreview(gtx layout.Context, s *session, area image.Point) {
	s.mu.Lock()
	img := s.preview
	s.mu.Unlock()
	if img == nil {
		return
	}
	pw := gtx.Dp(240)
	ph := pw * img.Bounds().Dy() / max(img.Bounds().Dx(), 1)
	at := image.Pt(area.X-pw-gtx.Dp(12), area.Y-ph-gtx.Dp(12))
	defer op.Offset(at).Push(gtx.Ops).Pop()
	gtx.Constraints = layout.Exact(image.Pt(pw, ph))
	defer clip.UniformRRect(image.Rectangle{Max: image.Pt(pw, ph)}, gtx.Dp(8)).Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, tileColor)
	pic := widget.Image{Src: paint.NewImageOp(img), Fit: widget.Contain, Position: layout.Center, Scale: 1 / gtx.Metric.PxPerDp}
	pic.Layout(gtx)
	layout.NW.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(6)).Layout(gtx, label(u.th, "You"))
	})
}

func (u *ui) loadSources() {
	u.sources = nil
	ds, ws, err := media.ListScreens()
	if err != nil {
		u.sources = []source{{name: "Can't list screens: " + err.Error()}}
	}
	for i, d := range ds {
		name := fmt.Sprintf("Display %d (%dx%d)", i+1, d.Width, d.Height)
		if d.Main {
			name += " — main"
		}
		u.sources = append(u.sources, source{name: name, display: d.ID})
	}
	for _, w := range ws {
		title := w.Title
		if title == "" {
			title = "(untitled)"
		}
		u.sources = append(u.sources, source{name: fmt.Sprintf("%s — %s", w.App, title), window: w.ID})
		if len(u.sources) >= 14 {
			break
		}
	}
	u.srcBtns = make([]widget.Clickable, len(u.sources))
}

// picker lists displays and windows to share.
func (u *ui) picker(gtx layout.Context) {
	defer op.Offset(image.Pt(gtx.Dp(8), gtx.Dp(8))).Push(gtx.Ops).Pop()
	gtx.Constraints.Min = image.Point{}
	gtx.Constraints.Max.X = gtx.Dp(420)
	layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(8))
			paint.FillShape(gtx.Ops, color.NRGBA{36, 39, 45, 250}, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				children := []layout.FlexChild{layout.Rigid(material.Body1(u.th, "Share which screen or window?").Layout)}
				for i := range u.sources {
					i := i
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min.X = gtx.Constraints.Max.X
							b := material.Button(u.th, &u.srcBtns[i], u.sources[i].name)
							b.Background = color.NRGBA{52, 56, 64, 255}
							b.TextSize = unit.Sp(13)
							return b.Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			})
		})
}

// writeSnapshot saves what the tiles and self-preview show (window
// content without the controls).
func writeSnapshot(path string, size image.Point, views []tileView, s *session) {
	img := image.NewRGBA(image.Rectangle{Max: size})
	draw.Draw(img, img.Bounds(), image.NewUniform(colorBG), image.Point{}, draw.Src)
	var info []string
	s.mu.Lock()
	for _, v := range views {
		if !v.Visible {
			continue
		}
		r := image.Rect(int(v.Rect.X), int(v.Rect.Y), int(v.Rect.X+v.Rect.W), int(v.Rect.Y+v.Rect.H))
		draw.Draw(img, r, image.NewUniform(colorTile), image.Point{}, draw.Src)
		if v.img != nil {
			drawFit(img, r, v.img)
		}
		info = append(info, v.label)
	}
	if p := s.preview; p != nil {
		w := size.X / 5
		h := w * p.Bounds().Dy() / max(p.Bounds().Dx(), 1)
		r := image.Rect(size.X-w-20, size.Y-h-20, size.X-20, size.Y-20)
		drawFit(img, r, p)
		info = append(info, "self-preview")
	}
	s.mu.Unlock()
	f, err := os.Create(path)
	if err != nil {
		log.Printf("snapshot: %v", err)
		return
	}
	defer f.Close()
	_ = png.Encode(f, img)
	log.Printf("snapshot written to %s: %s", path, strings.Join(info, "; "))
}
