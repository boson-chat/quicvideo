//go:build linux && cgo

package media

/*
#include <stdlib.h>
#include "media_linux.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"runtime/cgo"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// ScreenPermission: on Linux the xdg-desktop-portal asks the user to pick
// what to share each time (or reuses a stored choice), so it's always
// "undetermined" until a share starts.
func ScreenPermission() Permission { return PermissionUndetermined }

// RequestScreenPermission is a no-op: the portal asks when sharing starts.
func RequestScreenPermission() {}

// CameraPermission: camera access on Linux is device-file access
// (usually the "video" group).
func CameraPermission() Permission {
	devs := v4l2Devices()
	if len(devs) == 0 {
		return PermissionUndetermined
	}
	for _, d := range devs {
		if f, err := os.OpenFile(d.ID, os.O_RDWR, 0); err == nil {
			f.Close()
			return PermissionGranted
		}
	}
	return PermissionDenied
}

func RequestCameraPermission(time.Duration) Permission { return CameraPermission() }

func v4l2Devices() []Camera {
	cs := C.qv_v4l2_list()
	defer C.free(unsafe.Pointer(cs))
	var out []Camera
	for _, line := range strings.Split(C.GoString(cs), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		kind := CameraExternal
		if len(parts) == 3 && !strings.HasPrefix(parts[2], "usb") {
			// Not on USB: built-in (laptop cameras are often USB too) or a
			// v4l2loopback device (bus "platform:v4l2loopback-...").
			kind = CameraBuiltIn
			if strings.Contains(parts[2], "loopback") {
				kind = CameraVirtual
			}
		}
		out = append(out, Camera{ID: parts[0], Name: parts[1], Kind: kind})
	}
	return out
}

// ListCameras lists V4L2 capture devices (/dev/video*).
func ListCameras() ([]Camera, error) { return v4l2Devices(), nil }

func cameraPermissionError(p Permission) error {
	return &PermissionError{What: "Camera", State: p, Help: "Make sure your user can open /dev/video* (usually: be in the \"video\" group)."}
}

type capSource struct {
	w, h, fps int
	push      func(CapturedFrame)
	handle    cgo.Handle
	stopFn    func()
	once      sync.Once
	mu        sync.Mutex
	stopped   bool
	ended
}

func (s *capSource) Size() (int, int) { s.mu.Lock(); defer s.mu.Unlock(); return s.w, s.h }
func (s *capSource) FPS() int         { return s.fps }
func (s *capSource) Stop() {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.stopFn()
		s.handle.Delete()
	})
}

//export qvGoCapFrame
func qvGoCapFrame(handle C.uintptr_t, f *C.AVFrame, us C.int64_t) {
	s := cgo.Handle(handle).Value().(*capSource)
	fr := wrapFrame(f)
	defer fr.Release()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.w, s.h = fr.Size()
	s.mu.Unlock()
	s.push(CapturedFrame{Picture: fr, CaptureUS: uint64(us)})
}

//export qvGoCapStopped
func qvGoCapStopped(handle C.uintptr_t, msg *C.char) {
	if msg == nil {
		return
	}
	defer C.free(unsafe.Pointer(msg))
	s, ok := cgo.Handle(handle).Value().(*capSource)
	if !ok {
		return
	}
	s.mu.Lock()
	stopping := s.stopped
	s.mu.Unlock()
	if !stopping { // our own Stop closes the stream too: not an end
		s.end(fmt.Errorf("media: capture stopped: %s", C.GoString(msg)))
	}
}

func startAV(format, url string, w, h, fps int, push func(CapturedFrame)) (Source, error) {
	s := &capSource{fps: fps, push: push, ended: newEnded()}
	s.handle = cgo.NewHandle(s)
	cf, cu := C.CString(format), C.CString(url)
	defer C.free(unsafe.Pointer(cf))
	defer C.free(unsafe.Pointer(cu))
	var errp *C.char
	c := C.qv_avcap_start(cf, cu, C.int(w), C.int(h), C.int(fps), C.uintptr_t(s.handle), &errp)
	if c == nil {
		s.handle.Delete()
		return nil, cerr(errp)
	}
	s.stopFn = func() { C.qv_avcap_stop(c) }
	return s, nil
}

// StartCamera captures from a V4L2 camera (MJPEG preferred: most webcams
// only reach HD at 30 fps that way).
func StartCamera(o CameraOptions, push func(CapturedFrame)) (Source, error) {
	if o.Width <= 0 || o.Height <= 0 {
		o.Width, o.Height = 1280, 720
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	if o.ID == "" {
		c, ok := DefaultCamera(v4l2Devices())
		if !ok {
			return nil, errors.New("media: no camera found (/dev/video*)")
		}
		o.ID = c.ID
	}
	if f, err := os.OpenFile(o.ID, os.O_RDWR, 0); err != nil {
		if os.IsPermission(err) {
			return nil, cameraPermissionError(PermissionDenied)
		}
		return nil, fmt.Errorf("media: %w", err)
	} else {
		f.Close()
	}
	return startAV("video4linux2", o.ID, o.Width, o.Height, o.FPS, push)
}

// ListScreens: on Wayland the portal's own picker chooses (apps can't
// enumerate screens), so this returns one entry meaning "ask the system";
// on X11 it returns the X display.
func ListScreens() ([]Display, []Window, error) {
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") != "" {
		return []Display{{ID: 1, Main: true}}, nil, nil
	}
	return []Display{{ID: PortalPicker, Main: true}}, nil, nil
}

// StartScreen shares the screen. On Wayland it goes through the
// xdg-desktop-portal ScreenCast picker (the choice is remembered with a
// restore token, so later shares don't ask again); on X11 it uses x11grab.
func StartScreen(o ScreenOptions, push func(CapturedFrame)) (Source, error) {
	if o.FPS <= 0 {
		o.FPS = 15
	}
	if o.MaxWidth <= 0 {
		o.MaxWidth = 1920
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") != "" {
		return startAV("x11grab", os.Getenv("DISPLAY"), 0, 0, o.FPS, push)
	}
	sess, err := portalScreenCast(o.WindowID != 0, o.Cursor)
	if err != nil {
		return nil, err
	}
	s := &capSource{fps: o.FPS, push: push, w: sess.width, h: sess.height, ended: newEnded()}
	s.handle = cgo.NewHandle(s)
	var errp *C.char
	c := C.qv_pwcap_start(C.int(sess.fd), C.uint32_t(sess.node), C.int(o.MaxWidth), C.int(o.FPS), C.uintptr_t(s.handle), &errp)
	if c == nil {
		s.handle.Delete()
		sess.close()
		return nil, cerr(errp)
	}
	s.stopFn = func() {
		C.qv_pwcap_stop(c)
		sess.close()
	}
	return s, nil
}
