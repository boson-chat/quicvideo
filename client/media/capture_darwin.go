//go:build darwin

package media

/*
#include <stdlib.h>
#include "media_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"
)

// ScreenPermission reports whether this process may record the screen.
// macOS can't tell "not asked yet" from "denied" without asking, so a
// missing permission is reported as undetermined.
func ScreenPermission() Permission {
	if C.qv_screen_access() == 1 {
		return PermissionGranted
	}
	return PermissionUndetermined
}

// RequestScreenPermission shows the system prompt (at most once per app).
// Even when the user allows it, it only takes effect after the app is
// restarted.
func RequestScreenPermission() { C.qv_screen_request() }

func cameraState(v C.int) Permission {
	switch v {
	case 1:
		return PermissionGranted
	case 0:
		return PermissionDenied
	}
	return PermissionUndetermined
}

// CameraPermission reports the camera permission without asking.
func CameraPermission() Permission { return cameraState(C.qv_camera_access()) }

// RequestCameraPermission asks (if not decided yet) and waits up to
// timeout for the user's answer.
func RequestCameraPermission(timeout time.Duration) Permission {
	return cameraState(C.qv_camera_request(C.int(timeout.Milliseconds())))
}

func screenPermissionError() error {
	return &PermissionError{What: "Screen Recording", State: ScreenPermission(), Help: screenHelp}
}

// ListScreens lists displays and normal on-screen windows.
func ListScreens() ([]Display, []Window, error) {
	if ScreenPermission() != PermissionGranted {
		return nil, nil, screenPermissionError()
	}
	var errp *C.char
	js := C.qv_screen_list(&errp)
	if js == nil {
		return nil, nil, cerr(errp)
	}
	defer C.free(unsafe.Pointer(js))
	var v struct {
		Displays []struct {
			ID            uint32
			Width, Height int
			Main          bool
		}
		Windows []struct {
			ID            uint32
			Title, App    string
			Width, Height int
		}
	}
	if err := json.Unmarshal([]byte(C.GoString(js)), &v); err != nil {
		return nil, nil, err
	}
	var ds []Display
	for _, d := range v.Displays {
		ds = append(ds, Display(d))
	}
	var ws []Window
	for _, w := range v.Windows {
		ws = append(ws, Window(w))
	}
	return ds, ws, nil
}

type nativeSource struct {
	w, h, fps int
	push      func(CapturedFrame)
	handle    cgo.Handle
	stopFn    func()
	once      sync.Once
	mu        sync.Mutex
	stopped   bool
	ended
}

func (s *nativeSource) Size() (int, int) { s.mu.Lock(); defer s.mu.Unlock(); return s.w, s.h }
func (s *nativeSource) FPS() int         { return s.fps }
func (s *nativeSource) Stop() {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.stopFn()
		s.handle.Delete()
	})
}

func (s *nativeSource) frame(pb unsafe.Pointer, us int64) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	p := wrapPB(pb)
	s.w, s.h = p.Size()
	s.mu.Unlock()
	s.push(CapturedFrame{Picture: p, CaptureUS: uint64(us)})
}

// StartScreen captures a display or window (ScreenCaptureKit). Frames only
// arrive when something on screen changed.
func StartScreen(o ScreenOptions, push func(CapturedFrame)) (Source, error) {
	if ScreenPermission() != PermissionGranted {
		RequestScreenPermission()
		return nil, screenPermissionError()
	}
	if o.FPS <= 0 {
		o.FPS = 15
	}
	if o.MaxWidth <= 0 {
		o.MaxWidth = 1920
	}
	if o.DisplayID == PortalPicker {
		o.DisplayID = 0 // no system picker here: the main display
	}
	w, h := 0, 0
	if o.WindowID == 0 {
		ds, _, err := ListScreens()
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			if (o.DisplayID == 0 && d.Main) || d.ID == o.DisplayID {
				w, h = fit(d.Width, d.Height, o.MaxWidth, o.MaxWidth)
			}
		}
	}
	s := &nativeSource{fps: o.FPS, push: push, w: w, h: h, ended: newEnded()}
	s.handle = cgo.NewHandle(s)
	var errp *C.char
	sc := C.qv_screen_start(C.uint32_t(o.DisplayID), C.uint32_t(o.WindowID), C.int(w), C.int(h), C.int(o.FPS),
		cbool(o.Cursor), C.uintptr_t(s.handle), &errp)
	if sc == nil {
		s.handle.Delete()
		return nil, cerr(errp)
	}
	s.stopFn = func() { C.qv_screen_stop(sc) }
	return s, nil
}

// sourceFor resolves a callback's handle. A callback racing with Stop
// (which deletes the handle once the native side is drained) finds none.
func sourceFor(handle C.uintptr_t) (s *nativeSource) {
	defer func() {
		if recover() != nil {
			s = nil
		}
	}()
	s, _ = cgo.Handle(handle).Value().(*nativeSource)
	return s
}

//export qvGoScreenFrame
func qvGoScreenFrame(handle C.uintptr_t, pb unsafe.Pointer, us C.int64_t) {
	if s := sourceFor(handle); s != nil {
		s.frame(pb, int64(us))
	}
}

//export qvGoScreenStopped
func qvGoScreenStopped(handle C.uintptr_t, code C.int, msg *C.char) {
	err := cerr(msg)
	s := sourceFor(handle)
	if s == nil {
		return
	}
	s.mu.Lock()
	stopping := s.stopped
	s.mu.Unlock()
	if !stopping {
		s.end(err)
	}
}

//export qvGoCameraStopped
func qvGoCameraStopped(handle C.uintptr_t, msg *C.char) {
	qvGoScreenStopped(handle, 0, msg)
}

func cameraPermissionError(p Permission) error {
	return &PermissionError{What: "Camera", State: p, Help: cameraHelp}
}

// ListCameras lists video capture devices (no permission needed).
func ListCameras() ([]Camera, error) {
	var errp *C.char
	js := C.qv_camera_list(&errp)
	if js == nil {
		return nil, cerr(errp)
	}
	defer C.free(unsafe.Pointer(js))
	var v []struct {
		ID, Name, Kind string
		Formats        []struct {
			Width, Height int
			MaxFPS        int `json:"max_fps"`
		}
	}
	if err := json.Unmarshal([]byte(C.GoString(js)), &v); err != nil {
		return nil, err
	}
	out := make([]Camera, 0, len(v))
	for _, c := range v {
		cam := Camera{ID: c.ID, Name: c.Name, Kind: CameraKind(c.Kind)}
		for _, f := range c.Formats {
			cam.Formats = append(cam.Formats, CameraFormat(f))
		}
		out = append(out, cam)
	}
	return out, nil
}

// StartCamera captures from a camera (AVFoundation). It asks for the
// Camera permission if needed, waiting up to 30 s for an answer.
func StartCamera(o CameraOptions, push func(CapturedFrame)) (Source, error) {
	if p := RequestCameraPermission(30 * time.Second); p != PermissionGranted {
		return nil, cameraPermissionError(p)
	}
	if o.Width <= 0 || o.Height <= 0 {
		o.Width, o.Height = 1280, 720
	}
	if o.ID == "" {
		// The system default is often a virtual camera (Immersed, OBS...);
		// prefer a real built-in one.
		if cams, err := ListCameras(); err == nil {
			if c, ok := DefaultCamera(cams); ok {
				o.ID = c.ID
			}
		}
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	s := &nativeSource{fps: o.FPS, push: push, ended: newEnded()}
	s.handle = cgo.NewHandle(s)
	id := C.CString(o.ID)
	defer C.free(unsafe.Pointer(id))
	var errp *C.char
	cam := C.qv_camera_start(id, C.int(o.Width), C.int(o.Height), C.int(o.FPS), C.uintptr_t(s.handle), &errp)
	if cam == nil {
		s.handle.Delete()
		return nil, cerr(errp)
	}
	s.stopFn = func() { C.qv_camera_stop(cam) }
	return s, nil
}

//export qvGoCameraFrame
func qvGoCameraFrame(handle C.uintptr_t, pb unsafe.Pointer, us C.int64_t) {
	if s := sourceFor(handle); s != nil {
		s.frame(pb, int64(us))
	}
}

// DefaultEncoder is the platform's hardware encoder factory.
var DefaultEncoder EncoderFactory = NewVTEncoder
