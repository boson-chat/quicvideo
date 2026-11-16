//go:build windows && cgo

package media

/*
#cgo LDFLAGS: -lmfplat -lmf -lmfreadwrite -lmfuuid -lwmcodecdspuuid -lstrmiids -luuid -lole32 -loleaut32 -ld3d11 -ldxgi -ldwmapi -lruntimeobject
#include <stdlib.h>
#include "media_windows.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"runtime/cgo"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// WinPicture is a top-down BGRA picture in C memory (captures and decoder
// output on Windows). It implements Picture.
type WinPicture struct {
	p    *C.qv_wpic
	refs atomic.Int32
}

func wrapWin(p *C.qv_wpic) *WinPicture {
	w := &WinPicture{p: p}
	w.refs.Store(1)
	return w
}

func (w *WinPicture) Size() (int, int) { return int(w.p.w), int(w.p.h) }
func (w *WinPicture) Retain()          { w.refs.Add(1) }
func (w *WinPicture) Release() {
	if w.refs.Add(-1) == 0 {
		C.qv_wpic_free(w.p)
		w.p = nil
	}
}

func (w *WinPicture) pix() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(w.p.data)), int(w.p.stride)*int(w.p.h))
}

// ToRGBA converts a picture to an RGBA image, scaled down to at most maxW
// pixels wide (0 = full size).
func ToRGBA(pic Picture, maxW int) (*image.RGBA, error) {
	w, ok := pic.(*WinPicture)
	if !ok {
		return nil, errors.New("media: ToRGBA needs a *WinPicture")
	}
	sw, sh := w.Size()
	dw, dh := sw, sh
	if maxW > 0 && sw > maxW {
		dw, dh = fit(sw, sh, maxW, maxW*sh/max(sw, 1)+2)
	}
	return bgraToRGBA(w.pix(), int(w.p.stride), sw, sh, dw, dh), nil
}

func cerr(e *C.char) error {
	if e == nil {
		return errors.New("media: unknown error")
	}
	defer C.free(unsafe.Pointer(e))
	return errors.New("media: " + C.GoString(e))
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// ---- encoder ---------------------------------------------------------------

// MFEncoder is a Media Foundation H.264 encoder (hardware MFT preferred,
// the Microsoft software encoder otherwise).
type MFEncoder struct {
	enc    *C.qv_wenc
	handle cgo.Handle
	out    func(EncodedFrame)
	name   string
	mu     sync.Mutex
	closed bool
}

// NewMFEncoder makes an encoder; preferHardware tries hardware MFTs first.
func NewMFEncoder(cfg EncoderConfig, preferHardware bool, out func(EncodedFrame)) (*MFEncoder, error) {
	e := &MFEncoder{out: out}
	e.handle = cgo.NewHandle(e)
	gop := max(int(cfg.GOP.Seconds()*float64(cfg.FPS)), 1)
	var errp *C.char
	name := make([]byte, 128)
	enc := C.qv_wenc_new(C.int(cfg.Width), C.int(cfg.Height), C.int(max(cfg.FPS, 1)), C.int(cfg.Bitrate), C.int(gop),
		cbool(cfg.Screen), cbool(preferHardware), C.uintptr_t(e.handle), &errp,
		(*C.char)(unsafe.Pointer(&name[0])), C.int(len(name)))
	if enc == nil {
		e.handle.Delete()
		return nil, cerr(errp)
	}
	e.enc = enc
	e.name = C.GoString((*C.char)(unsafe.Pointer(&name[0])))
	return e, nil
}

// Backend names the MFT in use.
func (e *MFEncoder) Backend() string { return "Media Foundation: " + e.name }

func (e *MFEncoder) Encode(p Picture, captureUS uint64, force bool) error {
	w, ok := p.(*WinPicture)
	if !ok {
		return errors.New("media: MFEncoder needs a *WinPicture")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("media: encoder closed")
	}
	if C.qv_wenc_encode(e.enc, w.p, C.int64_t(captureUS), cbool(force)) < 0 {
		return errors.New("media: encode failed")
	}
	return nil
}

func (e *MFEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	C.qv_wenc_close(e.enc)
	e.handle.Delete()
}

//export qvGoWencOutput
func qvGoWencOutput(handle C.uintptr_t, data *C.uint8_t, n C.int, key C.int, us C.int64_t) {
	e := cgo.Handle(handle).Value().(*MFEncoder)
	au := append([]byte{0, 0, 0, 1, nalAUD, 0xF0}, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(n))...)
	e.out(EncodedFrame{Data: au, Keyframe: key != 0, CaptureUS: uint64(us)})
}

// DefaultEncoder is the platform's best encoder factory.
var DefaultEncoder EncoderFactory = func(cfg EncoderConfig, out func(EncodedFrame)) (Encoder, error) {
	return NewMFEncoder(cfg, true, out)
}

// ---- decoder ---------------------------------------------------------------

type mfDecoder struct {
	d      *C.qv_wdec
	handle cgo.Handle
	out    func(Picture, uint64)
	mu     sync.Mutex
	closed bool
}

// NewVideoDecoder makes the platform's H.264 decoder (Media Foundation).
func NewVideoDecoder(out func(pic Picture, captureUS uint64)) (VideoDecoder, error) {
	d := &mfDecoder{out: out}
	d.handle = cgo.NewHandle(d)
	var errp *C.char
	d.d = C.qv_wdec_new(C.uintptr_t(d.handle), &errp)
	if d.d == nil {
		d.handle.Delete()
		return nil, cerr(errp)
	}
	return d, nil
}

func (d *mfDecoder) Decode(au []byte, captureUS uint64) error {
	if len(au) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("media: decoder closed")
	}
	if C.qv_wdec_decode(d.d, (*C.uint8_t)(unsafe.Pointer(&au[0])), C.int(len(au)), C.int64_t(captureUS)) < 0 {
		return errors.New("media: decode failed")
	}
	return nil
}

func (d *mfDecoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	C.qv_wdec_close(d.d)
	d.handle.Delete()
}

//export qvGoWdecOutput
func qvGoWdecOutput(handle C.uintptr_t, p *C.qv_wpic, us C.int64_t) {
	d := cgo.Handle(handle).Value().(*mfDecoder)
	w := wrapWin(p)
	d.out(w, uint64(us))
	w.Release()
}

// ---- capture ---------------------------------------------------------------

// Windows has no per-app screen capture permission (Windows.Graphics.Capture
// shows a capture border instead).
func ScreenPermission() Permission { return PermissionGranted }
func RequestScreenPermission()     {}

// CameraPermission: Windows decides when the camera is opened (Settings →
// Privacy & security → Camera); until then it's undetermined.
func CameraPermission() Permission                     { return PermissionUndetermined }
func RequestCameraPermission(time.Duration) Permission { return PermissionUndetermined }

const winCameraHelp = "Open Settings → Privacy & security → Camera and turn on \"Camera access\" and " +
	"\"Let desktop apps access your camera\"."

type winSource struct {
	w, h, fps int
	push      func(CapturedFrame)
	handle    cgo.Handle
	stopFn    func()
	once      sync.Once
	mu        sync.Mutex
	stopped   bool
}

func (s *winSource) Size() (int, int) { s.mu.Lock(); defer s.mu.Unlock(); return s.w, s.h }
func (s *winSource) FPS() int         { return s.fps }
func (s *winSource) Stop() {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.stopFn()
		s.handle.Delete()
	})
}

//export qvGoWcapFrame
func qvGoWcapFrame(handle C.uintptr_t, p *C.qv_wpic, us C.int64_t) {
	w := wrapWin(p)
	defer w.Release()
	s := cgo.Handle(handle).Value().(*winSource)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.w, s.h = w.Size()
	s.mu.Unlock()
	s.push(CapturedFrame{Picture: w, CaptureUS: uint64(us)})
}

// ListCameras lists Media Foundation video capture devices.
func ListCameras() ([]Camera, error) {
	var errp *C.char
	cs := C.qv_cam_list(&errp)
	if cs == nil {
		return nil, cerr(errp)
	}
	defer C.free(unsafe.Pointer(cs))
	var out []Camera
	for _, line := range strings.Split(C.GoString(cs), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		kind := CameraExternal
		link := strings.ToLower(parts[0])
		switch {
		case !strings.Contains(link, "usb#"):
			kind = CameraBuiltIn // integrated cameras sit on other buses (or report as such)
		}
		if strings.Contains(strings.ToLower(parts[1]), "virtual") || strings.Contains(link, "root#image") {
			kind = CameraVirtual
		}
		out = append(out, Camera{ID: parts[0], Name: parts[1], Kind: kind})
	}
	return out, nil
}

// StartCamera captures from a webcam (Media Foundation source reader).
func StartCamera(o CameraOptions, push func(CapturedFrame)) (Source, error) {
	if o.Width <= 0 || o.Height <= 0 {
		o.Width, o.Height = 1280, 720
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	if o.ID == "" {
		if cams, err := ListCameras(); err == nil {
			if c, ok := DefaultCamera(cams); ok {
				o.ID = c.ID
			}
		}
	}
	s := &winSource{fps: o.FPS, push: push}
	s.handle = cgo.NewHandle(s)
	id := C.CString(o.ID)
	defer C.free(unsafe.Pointer(id))
	var errp *C.char
	var denied C.int
	c := C.qv_cam_start(id, C.int(o.Width), C.int(o.Height), C.int(o.FPS), C.uintptr_t(s.handle), &errp, &denied)
	if c == nil {
		s.handle.Delete()
		err := cerr(errp)
		if denied != 0 {
			return nil, &PermissionError{What: "Camera", State: PermissionDenied, Help: winCameraHelp}
		}
		return nil, err
	}
	s.stopFn = func() { C.qv_cam_stop(c) }
	return s, nil
}

// ListScreens lists monitors (ID = index + 1) and top-level windows.
func ListScreens() ([]Display, []Window, error) {
	cs := C.qv_wgc_list()
	defer C.free(unsafe.Pointer(cs))
	var ds []Display
	var ws []Window
	for _, line := range strings.Split(C.GoString(cs), "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) < 5 {
			continue
		}
		w, _ := strconv.Atoi(f[2])
		h, _ := strconv.Atoi(f[3])
		switch f[0] {
		case "D":
			idx, _ := strconv.Atoi(f[1])
			ds = append(ds, Display{ID: uint32(idx + 1), Width: w, Height: h, Main: f[4] == "1"})
		case "W":
			id, _ := strconv.ParseUint(f[1], 10, 64)
			ws = append(ws, Window{ID: uint32(id), Title: f[4], Width: w, Height: h})
		}
	}
	return ds, ws, nil
}

// StartScreen captures a monitor or window with Windows.Graphics.Capture.
func StartScreen(o ScreenOptions, push func(CapturedFrame)) (Source, error) {
	if C.qv_wgc_supported() == 0 {
		return nil, fmt.Errorf("media: Windows.Graphics.Capture isn't supported here (Windows 10 1903 or later with a desktop session)")
	}
	if o.FPS <= 0 {
		o.FPS = 15
	}
	monitor := 0
	if o.DisplayID != 0 && o.DisplayID != PortalPicker {
		monitor = int(o.DisplayID) - 1
	}
	s := &winSource{fps: o.FPS, push: push}
	s.handle = cgo.NewHandle(s)
	var errp *C.char
	c := C.qv_wgc_start(C.int(monitor), C.uintptr_t(o.WindowID), C.int(o.FPS), cbool(o.Cursor), C.uintptr_t(s.handle), &errp)
	if c == nil {
		s.handle.Delete()
		return nil, cerr(errp)
	}
	s.stopFn = func() { C.qv_wgc_stop(c) }
	return s, nil
}

// ---- test pattern ----------------------------------------------------------

type winPattern struct {
	w, h, fps int
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

// newPatternPicture makes one test-pattern picture.
func newPatternPicture(w, h, n int) *WinPicture {
	p := wrapWin(C.qv_wpic_new(C.int(w), C.int(h)))
	drawPatternBGRA(p.pix(), int(p.p.stride), w, h, n)
	return p
}

// NewPatternSource starts a moving test pattern (no devices needed).
func NewPatternSource(w, h, fps int, push func(CapturedFrame)) (Source, error) {
	if w <= 0 || h <= 0 || fps <= 0 {
		return nil, errors.New("media: bad pattern size")
	}
	s := &winPattern{w: w, h: h, fps: fps, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Second / time.Duration(fps))
		defer t.Stop()
		for n := 0; ; n++ {
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
			p := newPatternPicture(w, h, n)
			push(CapturedFrame{Picture: p, CaptureUS: uint64(time.Now().UnixMicro())})
			p.Release()
		}
	}()
	return s, nil
}

func (s *winPattern) Size() (int, int) { return s.w, s.h }
func (s *winPattern) FPS() int         { return s.fps }
func (s *winPattern) Stop() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

// bgra2nv12 / nv122bgra expose the C conversions to tests.
func bgra2nv12roundtrip(src []byte, sw, sh, dw, dh int) []byte {
	ysize := dw * dh
	nv := make([]byte, ysize*3/2)
	C.qv_bgra_to_nv12((*C.uint8_t)(unsafe.Pointer(&src[0])), C.int(sw), C.int(sh), C.int(sw*4),
		(*C.uint8_t)(unsafe.Pointer(&nv[0])), C.int(dw), (*C.uint8_t)(unsafe.Pointer(&nv[ysize])), C.int(dw), C.int(dw), C.int(dh))
	out := make([]byte, dw*dh*4)
	C.qv_nv12_to_bgra((*C.uint8_t)(unsafe.Pointer(&nv[0])), C.int(dw), (*C.uint8_t)(unsafe.Pointer(&nv[ysize])), C.int(dw),
		C.int(dw), C.int(dh), (*C.uint8_t)(unsafe.Pointer(&out[0])), C.int(dw*4))
	return out
}

// SetDeviceOrientation is for Android; here the OS (or nothing) rotates frames.
func SetDeviceOrientation(int) {}
