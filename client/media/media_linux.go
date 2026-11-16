//go:build linux && !android && cgo

package media

/*
#cgo pkg-config: libavcodec libavformat libavdevice libavutil libswscale openh264
// PipeWire's pkg-config adds flags cgo doesn't allow (-fno-strict-overflow);
// its include paths are the same on every distro.
#cgo CFLAGS: -I/usr/include/pipewire-0.3 -I/usr/include/spa-0.2 -D_REENTRANT
#cgo LDFLAGS: -lpipewire-0.3
#include <stdlib.h>
#include "media_linux.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Frame is a picture backed by an FFmpeg AVFrame (capture or decoder
// output). It implements Picture.
type Frame struct {
	f    *C.AVFrame
	refs atomic.Int32
}

func wrapFrame(f *C.AVFrame) *Frame {
	fr := &Frame{f: f}
	fr.refs.Store(1)
	return fr
}

func (f *Frame) Size() (int, int) { return int(C.qv_frame_w(f.f)), int(C.qv_frame_h(f.f)) }
func (f *Frame) Retain()          { f.refs.Add(1) }
func (f *Frame) Release() {
	if f.refs.Add(-1) == 0 {
		C.qv_frame_free(f.f)
		f.f = nil
	}
}

// ToRGBA converts a picture to an RGBA image, scaled down to at most maxW
// pixels wide (0 = full size).
func ToRGBA(pic Picture, maxW int) (*image.RGBA, error) {
	f, ok := pic.(*Frame)
	if !ok {
		return nil, errors.New("media: ToRGBA needs a *Frame")
	}
	w, h := f.Size()
	if maxW > 0 && w > maxW {
		w, h = fit(w, h, maxW, maxW*h/max(w, 1)+2)
	}
	rgba := C.qv_frame_to_rgba(f.f, C.int(w), C.int(h))
	if rgba == nil {
		return nil, errors.New("media: pixel conversion failed")
	}
	defer C.qv_frame_free(rgba)
	var stride C.int
	data := C.qv_frame_data(rgba, 0, &stride)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	src := unsafe.Slice((*byte)(data), int(stride)*h)
	for y := range h {
		copy(img.Pix[y*img.Stride:y*img.Stride+w*4], src[y*int(stride):])
	}
	return img, nil
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

// Encoder backends on Linux.
const (
	BackendAuto     = 0 // NVENC, then VA-API, then openh264 (then x264 if allowed)
	BackendNVENC    = 1
	BackendVAAPI    = 2
	BackendOpenH264 = 3
	BackendX264     = 4
)

// AVEncoder is a Linux H.264 encoder for one layer.
type AVEncoder struct {
	enc     *C.qv_lenc
	handle  cgo.Handle
	out     func(EncodedFrame)
	backend string
	mu      sync.Mutex
	closed  bool
}

// NewAVEncoder makes an encoder with a specific backend (BackendAuto picks
// the best available). libx264 is GPL: it's only used when explicitly
// asked for, or with QUICVIDEO_ALLOW_GPL=1 for BackendAuto.
func NewAVEncoder(cfg EncoderConfig, backend int, out func(EncodedFrame)) (*AVEncoder, error) {
	e := &AVEncoder{out: out}
	e.handle = cgo.NewHandle(e)
	gop := max(int(cfg.GOP.Seconds()*float64(cfg.FPS)), 1)
	var errp *C.char
	name := make([]byte, 64)
	enc := C.qv_lenc_new(C.int(cfg.Width), C.int(cfg.Height), C.int(max(cfg.FPS, 1)), C.int(cfg.Bitrate), C.int(gop),
		cbool(cfg.Screen), C.int(backend), cbool(os.Getenv("QUICVIDEO_ALLOW_GPL") == "1"),
		C.uintptr_t(e.handle), &errp, (*C.char)(unsafe.Pointer(&name[0])), C.int(len(name)))
	if enc == nil {
		e.handle.Delete()
		return nil, cerr(errp)
	}
	e.enc = enc
	e.backend = C.GoString((*C.char)(unsafe.Pointer(&name[0])))
	return e, nil
}

// newAutoEncoder implements EncoderFactory.
func newAutoEncoder(cfg EncoderConfig, out func(EncodedFrame)) (Encoder, error) {
	return NewAVEncoder(cfg, BackendAuto, out)
}

// DefaultEncoder is the platform's best encoder factory.
var DefaultEncoder EncoderFactory = newAutoEncoder

// Backend names the encoder in use, e.g. "NVENC (h264_nvenc)".
func (e *AVEncoder) Backend() string { return e.backend }

func (e *AVEncoder) Encode(p Picture, captureUS uint64, force bool) error {
	f, ok := p.(*Frame)
	if !ok {
		return errors.New("media: AVEncoder needs a *Frame")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("media: encoder closed")
	}
	if r := C.qv_lenc_encode(e.enc, f.f, C.int64_t(captureUS), cbool(force)); r < 0 {
		return fmt.Errorf("media: encode failed (%d)", int(r))
	}
	return nil
}

func (e *AVEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	C.qv_lenc_close(e.enc)
	e.handle.Delete()
}

//export qvGoLencOutput
func qvGoLencOutput(handle C.uintptr_t, data *C.uint8_t, n C.int, key C.int, us C.int64_t) {
	e := cgo.Handle(handle).Value().(*AVEncoder)
	// Normalize to quicvideo's access unit: AUD first (encoders' Annex-B
	// output already carries SPS/PPS on IDR frames).
	au := append([]byte{0, 0, 0, 1, nalAUD, 0xF0}, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(n))...)
	e.out(EncodedFrame{Data: au, Keyframe: key != 0, CaptureUS: uint64(us)})
}

// ---- decoder ---------------------------------------------------------------

type avDecoder struct {
	d      *C.qv_ldec
	handle cgo.Handle
	out    func(Picture, uint64)
	mu     sync.Mutex
	closed bool
}

// NewVideoDecoder makes the platform's H.264 decoder (libavcodec).
func NewVideoDecoder(out func(pic Picture, captureUS uint64)) (VideoDecoder, error) {
	d := &avDecoder{out: out}
	d.handle = cgo.NewHandle(d)
	var errp *C.char
	d.d = C.qv_ldec_new(C.uintptr_t(d.handle), &errp)
	if d.d == nil {
		d.handle.Delete()
		return nil, cerr(errp)
	}
	return d, nil
}

func (d *avDecoder) Decode(au []byte, captureUS uint64) error {
	if len(au) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("media: decoder closed")
	}
	if r := C.qv_ldec_decode(d.d, (*C.uint8_t)(unsafe.Pointer(&au[0])), C.int(len(au)), C.int64_t(captureUS)); r < 0 {
		return fmt.Errorf("media: decode failed (%d)", int(r))
	}
	return nil
}

func (d *avDecoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	C.qv_ldec_close(d.d)
	d.handle.Delete()
}

//export qvGoLdecOutput
func qvGoLdecOutput(handle C.uintptr_t, f *C.AVFrame, us C.int64_t) {
	d := cgo.Handle(handle).Value().(*avDecoder)
	fr := wrapFrame(f)
	d.out(fr, uint64(us))
	fr.Release()
}

// ---- test pattern ----------------------------------------------------------

type patternSource struct {
	w, h, fps int
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

// NewPatternSource starts a moving test pattern (no devices needed).
func NewPatternSource(w, h, fps int, push func(CapturedFrame)) (Source, error) {
	if w <= 0 || h <= 0 || fps <= 0 {
		return nil, errors.New("media: bad pattern size")
	}
	s := &patternSource{w: w, h: h, fps: fps, stop: make(chan struct{}), done: make(chan struct{})}
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
			cf := C.qv_frame_new_bgra(C.int(w), C.int(h))
			if cf == nil {
				continue
			}
			var stride C.int
			data := C.qv_frame_data(cf, 0, &stride)
			drawPatternBGRA(unsafe.Slice((*byte)(data), int(stride)*h), int(stride), w, h, n)
			fr := wrapFrame(cf)
			push(CapturedFrame{Picture: fr, CaptureUS: uint64(time.Now().UnixMicro())})
			fr.Release()
		}
	}()
	return s, nil
}

func (s *patternSource) Size() (int, int) { return s.w, s.h }
func (s *patternSource) FPS() int         { return s.fps }
func (s *patternSource) Stop() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

// newPatternFrame makes one BGRA test-pattern frame (tests).
func newPatternFrame(w, h, n int) *Frame {
	cf := C.qv_frame_new_bgra(C.int(w), C.int(h))
	var stride C.int
	data := C.qv_frame_data(cf, 0, &stride)
	drawPatternBGRA(unsafe.Slice((*byte)(data), int(stride)*h), int(stride), w, h, n)
	return wrapFrame(cf)
}
