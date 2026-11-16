//go:build android && cgo

package media

/*
#cgo LDFLAGS: -lcamera2ndk -lmediandk -landroid
#include <stdlib.h>
#include "media_android.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Android: cameras through the Camera2 NDK and H.264 encoding through
// AMediaCodec (hardware). No screen sharing yet (MediaProjection needs a
// foreground service and the system's consent dialog, a later phase) and
// no decoder (the boson app decodes in its WebView).

// NV12 is a picture in Go memory: width*height luma, then interleaved UV.
type NV12 struct {
	W, H int
	Data []byte
}

func (p *NV12) Size() (int, int) { return p.W, p.H }
func (p *NV12) Retain()          {}
func (p *NV12) Release()         {}

func ScreenPermission() Permission { return PermissionDenied }
func RequestScreenPermission()     {}

// CameraPermission: the app asks for android.permission.CAMERA itself
// (a runtime permission needs the Activity); this package can't tell.
func CameraPermission() Permission { return PermissionUndetermined }

func RequestCameraPermission(time.Duration) Permission { return PermissionUndetermined }

func ListScreens() ([]Display, []Window, error) { return nil, nil, ErrUnsupported }

func StartScreen(ScreenOptions, func(CapturedFrame)) (Source, error) { return nil, ErrUnsupported }

func NewVideoDecoder(func(Picture, uint64)) (VideoDecoder, error) { return nil, ErrUnsupported }

func NewPatternSource(int, int, int, func(CapturedFrame)) (Source, error) { return nil, ErrUnsupported }

type acamInfo struct {
	ID          string
	Name        string
	Kind        string
	Facing      int
	Orientation int
	Formats     []struct {
		Width, Height int
		MaxFPS        int `json:"max_fps"`
	}
}

func listACams() ([]acamInfo, error) {
	var errp *C.char
	js := C.qv_acam_list(&errp)
	if js == nil {
		return nil, androidErr(errp)
	}
	defer C.free(unsafe.Pointer(js))
	var v []acamInfo
	if err := json.Unmarshal([]byte(C.GoString(js)), &v); err != nil {
		return nil, err
	}
	return v, nil
}

func androidErr(e *C.char) error {
	if e == nil {
		return errors.New("media: unknown error")
	}
	defer C.free(unsafe.Pointer(e))
	return errors.New("media: " + C.GoString(e))
}

// ListCameras lists the device's cameras, the front one first (a call's
// default).
func ListCameras() ([]Camera, error) {
	cams, err := listACams()
	if err != nil {
		return nil, err
	}
	out := make([]Camera, 0, len(cams))
	for _, front := range []bool{true, false} {
		for _, c := range cams {
			if (c.Facing == 0) != front {
				continue
			}
			cam := Camera{ID: c.ID, Name: c.Name, Kind: CameraKind(c.Kind), Facing: [...]string{"front", "back", ""}[min(max(c.Facing, 0), 2)]}
			for _, f := range c.Formats {
				cam.Formats = append(cam.Formats, CameraFormat{Width: f.Width, Height: f.Height, MaxFPS: f.MaxFPS})
			}
			out = append(out, cam)
		}
	}
	return out, nil
}

// acamSource is a running camera.
type acamSource struct {
	cam      *C.qv_acam
	handle   cgo.Handle
	push     func(CapturedFrame)
	fps      int
	rotation int // the sensor's orientation (degrees clockwise)
	front    bool

	mu      sync.Mutex
	w, h    int
	stopped bool
	once    sync.Once
	ended
}

func (s *acamSource) Size() (int, int) { s.mu.Lock(); defer s.mu.Unlock(); return s.w, s.h }
func (s *acamSource) FPS() int         { return s.fps }
func (s *acamSource) Stop() {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		C.qv_acam_stop(s.cam)
		s.handle.Delete()
	})
}

// StartCamera opens a camera (ID "" = the front one) at about the asked
// size and pushes upright NV12 pictures (the sensor's orientation is
// undone: a phone held upright sends portrait video).
func StartCamera(o CameraOptions, push func(CapturedFrame)) (Source, error) {
	cams, err := listACams()
	if err != nil {
		return nil, err
	}
	var pick *acamInfo
	for i := range cams {
		if cams[i].ID == o.ID || (o.ID == "" && pick == nil && cams[i].Facing == 0) {
			pick = &cams[i]
		}
	}
	if pick == nil && o.ID == "" && len(cams) > 0 {
		pick = &cams[0]
	}
	if pick == nil {
		return nil, errors.New("media: no camera found")
	}
	if o.Width <= 0 || o.Height <= 0 {
		o.Width, o.Height = 1280, 720
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	// The listed size closest to the request (cameras only do their own).
	w, h := o.Width, o.Height
	best := -1
	for _, f := range pick.Formats {
		d := abs(f.Width*f.Height - o.Width*o.Height)
		if best < 0 || d < best {
			best, w, h = d, f.Width, f.Height
		}
	}
	s := &acamSource{push: push, fps: o.FPS, rotation: pick.Orientation % 360, front: pick.Facing == 0, ended: newEnded()}
	s.handle = cgo.NewHandle(s)
	id := C.CString(pick.ID)
	defer C.free(unsafe.Pointer(id))
	var errp *C.char
	cam := C.qv_acam_start(id, C.int(w), C.int(h), C.int(o.FPS), C.uintptr_t(s.handle), &errp)
	if cam == nil {
		s.handle.Delete()
		err := androidErr(errp)
		if errors.Is(err, errPermission) || err.Error() == "media: camera permission denied" {
			return nil, cameraPermissionError(PermissionDenied)
		}
		return nil, err
	}
	s.cam = cam
	return s, nil
}

var errPermission = errors.New("media: camera permission denied")

// deviceOrientation is how far the phone is turned clockwise from upright
// portrait (0, 90, 180, 270), set by the app from its orientation
// listener (SetDeviceOrientation); 0 when it doesn't.
var deviceOrientation atomic.Int32

// SetDeviceOrientation tells the camera how the phone is held (degrees
// clockwise from portrait, any value: rounded to the nearest quarter
// turn), so a phone turned on its side still sends upright video.
func SetDeviceOrientation(deg int) {
	deg = ((deg % 360) + 360) % 360
	deviceOrientation.Store(int32(((deg + 45) / 90 * 90) % 360))
}

func cameraPermissionError(p Permission) error {
	return &PermissionError{What: "Camera", State: p, Help: "allow it in Settings → Apps → Boson → Permissions"}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sourceForHandle(h C.uintptr_t) (s *acamSource) {
	defer func() {
		if recover() != nil {
			s = nil
		}
	}()
	s, _ = cgo.Handle(h).Value().(*acamSource)
	return s
}

//export qvGoACamFrame
func qvGoACamFrame(h C.uintptr_t, y *C.uint8_t, ystride C.int, u, v *C.uint8_t, uvstride, uvpixel, width, height C.int, us C.int64_t) {
	s := sourceForHandle(h)
	if s == nil {
		return
	}
	w, hh := int(width), int(height)
	ys, uvs, uvp := int(ystride), int(uvstride), int(uvpixel)
	yPlane := unsafe.Slice((*byte)(unsafe.Pointer(y)), ys*(hh-1)+w)
	uvLen := uvs*(hh/2-1) + (w/2-1)*uvp + 1
	uPlane := unsafe.Slice((*byte)(unsafe.Pointer(u)), uvLen)
	vPlane := unsafe.Slice((*byte)(unsafe.Pointer(v)), uvLen)
	pic := yuvToNV12Rotated(yPlane, ys, uPlane, vPlane, uvs, uvp, w, hh, uprightRotation(s.rotation, int(deviceOrientation.Load()), s.front))
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.w, s.h = pic.W, pic.H
	s.mu.Unlock()
	s.push(CapturedFrame{Picture: pic, CaptureUS: uint64(us)})
}

//export qvGoACamStopped
func qvGoACamStopped(h C.uintptr_t, msg *C.char) {
	err := androidErr(msg)
	if s := sourceForHandle(h); s != nil {
		s.mu.Lock()
		stopping := s.stopped
		s.mu.Unlock()
		if !stopping {
			s.end(err)
		}
	}
}

// yuvToNV12Rotated copies a YUV_420_888 image (any plane strides) into NV12
// and rotates it by rot (0/90/180/270, clockwise).
func yuvToNV12Rotated(y []byte, ys int, u, v []byte, uvs, uvp, w, h, rot int) *NV12 {
	ow, oh := w, h
	if rot == 90 || rot == 270 {
		ow, oh = h, w
	}
	ow, oh = ow&^1, oh&^1
	out := &NV12{W: ow, H: oh, Data: make([]byte, ow*oh*3/2)}
	luma := out.Data[:ow*oh]
	chroma := out.Data[ow*oh:]
	// map output (x, y) → source (sx, sy)
	src := func(x, yy int) (int, int) {
		switch rot {
		case 90:
			return yy, h - 1 - x
		case 180:
			return w - 1 - x, h - 1 - yy
		case 270:
			return w - 1 - yy, x
		}
		return x, yy
	}
	for oy := 0; oy < oh; oy++ {
		row := luma[oy*ow : oy*ow+ow]
		for ox := 0; ox < ow; ox++ {
			sx, sy := src(ox, oy)
			row[ox] = y[sy*ys+sx]
		}
	}
	for oy := 0; oy < oh/2; oy++ {
		row := chroma[oy*ow : oy*ow+ow]
		for ox := 0; ox < ow/2; ox++ {
			sx, sy := src(ox*2, oy*2)
			i := (sy/2)*uvs + (sx/2)*uvp
			row[ox*2] = u[i]
			row[ox*2+1] = v[i]
		}
	}
	return out
}

// scaleNV12 resizes (nearest neighbour; layers are 1/2 and 1/4 sizes).
func scaleNV12(p *NV12, w, h int) *NV12 {
	w, h = w&^1, h&^1
	if p.W == w && p.H == h {
		return p
	}
	out := &NV12{W: w, H: h, Data: make([]byte, w*h*3/2)}
	for y := 0; y < h; y++ {
		sy := y * p.H / h
		for x := 0; x < w; x++ {
			out.Data[y*w+x] = p.Data[sy*p.W+x*p.W/w]
		}
	}
	src := p.Data[p.W*p.H:]
	dst := out.Data[w*h:]
	for y := 0; y < h/2; y++ {
		sy := y * (p.H / 2) / (h / 2)
		for x := 0; x < w/2; x++ {
			sx := x * (p.W / 2) / (w / 2)
			dst[y*w+x*2] = src[sy*p.W+sx*2]
			dst[y*w+x*2+1] = src[sy*p.W+sx*2+1]
		}
	}
	return out
}

// ToRGBA converts (and shrinks to at most maxW wide) an NV12 picture.
func ToRGBA(pic Picture, maxW int) (*image.RGBA, error) {
	p, ok := pic.(*NV12)
	if !ok {
		return nil, errors.New("media: ToRGBA needs an *NV12")
	}
	w, h := p.W, p.H
	if maxW > 0 && w > maxW {
		h = h * maxW / w
		w = maxW
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	uv := p.Data[p.W*p.H:]
	for y := 0; y < h; y++ {
		sy := y * p.H / h
		for x := 0; x < w; x++ {
			sx := x * p.W / w
			Y := float64(p.Data[sy*p.W+sx]) - 16
			i := (sy/2)*p.W + (sx/2)*2
			U := float64(uv[i]) - 128
			V := float64(uv[i+1]) - 128
			r := 1.164*Y + 1.596*V
			g := 1.164*Y - 0.392*U - 0.813*V
			b := 1.164*Y + 2.017*U
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = clamp8(r), clamp8(g), clamp8(b), 255
		}
	}
	return img, nil
}

func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// ---- encoder ----

// AEncoder is AMediaCodec's H.264 encoder for one layer.
type AEncoder struct {
	enc    *C.qv_aenc
	handle cgo.Handle
	out    func(EncodedFrame)
	w, h   int
	mu     sync.Mutex
	closed bool
}

// DefaultEncoder is the platform's hardware encoder factory.
var DefaultEncoder EncoderFactory = func(cfg EncoderConfig, out func(EncodedFrame)) (Encoder, error) {
	return NewAEncoder(cfg, out)
}

func NewAEncoder(cfg EncoderConfig, out func(EncodedFrame)) (*AEncoder, error) {
	e := &AEncoder{out: out, w: cfg.Width &^ 1, h: cfg.Height &^ 1}
	e.handle = cgo.NewHandle(e)
	gop := int(cfg.GOP.Seconds())
	var errp *C.char
	enc := C.qv_aenc_new(C.int(e.w), C.int(e.h), C.int(max(cfg.FPS, 1)), C.int(cfg.Bitrate), C.int(gop), C.uintptr_t(e.handle), &errp)
	if enc == nil {
		e.handle.Delete()
		return nil, androidErr(errp)
	}
	e.enc = enc
	return e, nil
}

func (e *AEncoder) Encode(p Picture, captureUS uint64, force bool) error {
	pic, ok := p.(*NV12)
	if !ok {
		return errors.New("media: AEncoder needs an *NV12")
	}
	pic = scaleNV12(pic, e.w, e.h)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("media: encoder closed")
	}
	if r := C.qv_aenc_encode(e.enc, (*C.uint8_t)(unsafe.Pointer(&pic.Data[0])), C.int64_t(captureUS), cbool(force)); r < 0 {
		return fmt.Errorf("media: encode failed (%d)", int(r))
	}
	return nil
}

func (e *AEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	C.qv_aenc_close(e.enc)
	e.handle.Delete()
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

//export qvGoAEncOut
func qvGoAEncOut(h C.uintptr_t, data *C.uint8_t, size C.size_t, key C.int, us C.int64_t) {
	e, ok := cgo.Handle(h).Value().(*AEncoder)
	if !ok {
		return
	}
	b := C.GoBytes(unsafe.Pointer(data), C.int(size))
	e.out(EncodedFrame{Data: b, Keyframe: key != 0, CaptureUS: uint64(us)})
}
