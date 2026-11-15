//go:build darwin

package media

/*
#cgo CFLAGS: -Wno-unused-command-line-argument -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework Foundation -framework CoreMedia -framework CoreVideo -framework VideoToolbox -framework CoreGraphics -framework IOSurface -framework ScreenCaptureKit -framework AVFoundation -framework AppKit
#include <stdlib.h>
#include "media_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"
)

// PixelBuffer is a CVPixelBuffer (capture or decoder output). It
// implements Picture.
type PixelBuffer struct{ ref unsafe.Pointer }

func wrapPB(ref unsafe.Pointer) *PixelBuffer { return &PixelBuffer{ref: ref} }

func (p *PixelBuffer) Size() (int, int) { return int(C.qv_pb_width(p.ref)), int(C.qv_pb_height(p.ref)) }
func (p *PixelBuffer) Retain()          { C.qv_pb_retain(p.ref) }
func (p *PixelBuffer) Release()         { C.qv_pb_release(p.ref) }

// Ref is the CVPixelBufferRef (for display code).
func (p *PixelBuffer) Ref() unsafe.Pointer { return p.ref }

// IOSurface returns the buffer's IOSurfaceRef (nil if it has none); a
// CALayer can show it directly as its contents.
func (p *PixelBuffer) IOSurface() unsafe.Pointer { return C.qv_pb_iosurface(p.ref) }

// lock maps the pixels; call the returned func to unlock.
func (p *PixelBuffer) lock(readonly bool) ([]byte, int, func()) {
	var stride C.int
	base := C.qv_pb_lock(p.ref, cbool(readonly), &stride)
	_, h := p.Size()
	return unsafe.Slice((*byte)(base), int(stride)*h), int(stride), func() { C.qv_pb_unlock(p.ref, cbool(readonly)) }
}

// ToRGBA converts a picture (any pixel format, e.g. an NV12 capture) to
// an RGBA image, scaled down to at most maxW pixels wide (0 = full size).
func ToRGBA(pic Picture, maxW int) (*image.RGBA, error) {
	p, ok := pic.(*PixelBuffer)
	if !ok {
		return nil, errors.New("media: ToRGBA needs a *PixelBuffer")
	}
	w, h := p.Size()
	if maxW > 0 && w > maxW {
		w, h = fit(w, h, maxW, maxW*h/max(w, 1)+2)
	}
	if pw, ph := p.Size(); pw == w && ph == h && uint32(C.qv_pb_format(p.ref)) == fourccBGRA {
		return p.RGBA()
	}
	ref := C.qv_pb_to_bgra(p.ref, C.int(w), C.int(h))
	if ref == nil {
		return nil, errors.New("media: pixel conversion failed")
	}
	tmp := wrapPB(ref)
	defer tmp.Release()
	return tmp.RGBA()
}

// NewVideoDecoder makes the platform's H.264 decoder (VideoToolbox).
func NewVideoDecoder(out func(pic Picture, captureUS uint64)) (VideoDecoder, error) {
	return NewDecoder(func(pb *PixelBuffer, us uint64) { out(pb, us) })
}

// RGBA copies a BGRA buffer into an image (tests, screenshots).
func (p *PixelBuffer) RGBA() (*image.RGBA, error) {
	if f := uint32(C.qv_pb_format(p.ref)); f != fourccBGRA {
		return nil, fmt.Errorf("media: RGBA needs a BGRA buffer, have %q", fourcc(f))
	}
	src, stride, unlock := p.lock(true)
	defer unlock()
	w, h := p.Size()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		row := src[y*stride : y*stride+w*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w*4; x += 4 {
			dst[x], dst[x+1], dst[x+2], dst[x+3] = row[x+2], row[x+1], row[x], 255
		}
	}
	return img, nil
}

const fourccBGRA = 'B'<<24 | 'G'<<16 | 'R'<<8 | 'A'

func fourcc(v uint32) string {
	return string([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func cerr(e *C.char) error {
	if e == nil {
		return errors.New("media: unknown error")
	}
	defer C.free(unsafe.Pointer(e))
	return errors.New("media: " + C.GoString(e))
}

// ---- encoder ---------------------------------------------------------------

// VTEncoder is a VideoToolbox H.264 encoder for one layer.
type VTEncoder struct {
	enc      *C.qv_enc
	handle   cgo.Handle
	out      func(EncodedFrame)
	hardware bool
	onError  func(error)
	buf      []byte
	mu       sync.Mutex
	closed   bool
}

// NewVTEncoder implements EncoderFactory with VideoToolbox: hardware H.264
// with low-latency rate control, no B-frames, keyframes at most every GOP.
func NewVTEncoder(cfg EncoderConfig, out func(EncodedFrame)) (Encoder, error) {
	e := &VTEncoder{out: out}
	e.handle = cgo.NewHandle(e)
	gop := max(int(cfg.GOP.Seconds()*float64(cfg.FPS)), 1)
	var errp *C.char
	var hw C.int
	enc := C.qv_enc_new(C.int(cfg.Width), C.int(cfg.Height), C.int(max(cfg.FPS, 1)), C.int(cfg.Bitrate), C.int(gop),
		cbool(cfg.Screen), C.uintptr_t(e.handle), &errp, &hw)
	if enc == nil {
		e.handle.Delete()
		return nil, cerr(errp)
	}
	e.enc, e.hardware = enc, hw != 0
	return e, nil
}

// Hardware reports whether a hardware encoder is in use.
func (e *VTEncoder) Hardware() bool { return e.hardware }

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func (e *VTEncoder) Encode(p Picture, captureUS uint64, force bool) error {
	pb, ok := p.(*PixelBuffer)
	if !ok {
		return errors.New("media: VTEncoder needs a *PixelBuffer")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("media: encoder closed")
	}
	if st := C.qv_enc_encode(e.enc, pb.ref, C.int64_t(captureUS), cbool(force)); st != 0 {
		return fmt.Errorf("media: encode failed (OSStatus %d)", int(st))
	}
	return nil
}

func (e *VTEncoder) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	C.qv_enc_close(e.enc) // flushes: remaining output arrives before it returns
	e.handle.Delete()
}

//export qvGoEncOutput
func qvGoEncOutput(handle C.uintptr_t, avcc unsafe.Pointer, n, lenSize C.int, sps unsafe.Pointer, spsLen C.int,
	pps unsafe.Pointer, ppsLen C.int, key C.int, us C.int64_t) {
	e := cgo.Handle(handle).Value().(*VTEncoder)
	var params [][]byte
	if key != 0 && sps != nil && pps != nil {
		params = [][]byte{C.GoBytes(sps, spsLen), C.GoBytes(pps, ppsLen)}
	}
	data, err := AVCCToAnnexB(nil, unsafe.Slice((*byte)(avcc), int(n)), int(lenSize), params, key != 0)
	if err != nil {
		return
	}
	e.out(EncodedFrame{Data: data, Keyframe: key != 0, CaptureUS: uint64(us)})
}

//export qvGoEncError
func qvGoEncError(handle C.uintptr_t, status C.int) {
	e := cgo.Handle(handle).Value().(*VTEncoder)
	if e.onError != nil {
		e.onError(fmt.Errorf("media: encoder error (OSStatus %d)", int(status)))
	}
}

// ---- decoder ---------------------------------------------------------------

// Decoder decodes one remote track's H.264 to BGRA pixel buffers. It
// follows resolution changes (layer switches) at keyframes.
type Decoder struct {
	dec    *C.qv_dec
	handle cgo.Handle
	out    func(*PixelBuffer, uint64)
	mu     sync.Mutex
	ready  bool // has seen parameter sets
	closed bool
}

// ErrNeedKeyframe: the decoder has no parameter sets yet (start at a
// group start).
var ErrNeedKeyframe = errors.New("media: decoder needs a keyframe")

// NewDecoder makes a decoder. out gets each picture (valid during the call;
// Retain it to keep it) with its capture time.
func NewDecoder(out func(pic *PixelBuffer, captureUS uint64)) (*Decoder, error) {
	d := &Decoder{out: out}
	d.handle = cgo.NewHandle(d)
	d.dec = C.qv_dec_new(C.uintptr_t(d.handle))
	return d, nil
}

// Decode decodes one Annex-B access unit. Output is synchronous.
func (d *Decoder) Decode(au []byte, captureUS uint64) error {
	a := AnnexBToAVCC(au)
	if len(a.AVCC) == 0 {
		return nil // parameter sets / AUD only
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("media: decoder closed")
	}
	if !d.ready && (a.SPS == nil || a.PPS == nil) {
		return ErrNeedKeyframe
	}
	var sps, pps unsafe.Pointer
	if a.SPS != nil && a.PPS != nil {
		sps, pps = C.CBytes(a.SPS), C.CBytes(a.PPS)
		defer C.free(sps)
		defer C.free(pps)
		d.ready = true
	}
	payload := C.CBytes(a.AVCC)
	defer C.free(payload)
	if st := C.qv_dec_decode(d.dec, sps, C.int(len(a.SPS)), pps, C.int(len(a.PPS)), payload, C.int(len(a.AVCC)),
		C.int64_t(captureUS)); st != 0 {
		return fmt.Errorf("media: decode failed (OSStatus %d)", int(st))
	}
	return nil
}

func (d *Decoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	C.qv_dec_close(d.dec)
	d.handle.Delete()
}

//export qvGoDecOutput
func qvGoDecOutput(handle C.uintptr_t, pb unsafe.Pointer, us C.int64_t) {
	d := cgo.Handle(handle).Value().(*Decoder)
	d.out(wrapPB(pb), uint64(us))
}

// ---- test pattern ----------------------------------------------------------

// PatternSource produces a moving test pattern (no permissions needed):
// colour bars, a sweeping box and a frame counter as a binary row.
type PatternSource struct {
	w, h, fps int
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

// NewPatternSource starts a w×h test pattern at fps, calling push for every
// frame (the picture is only valid during the call).
func NewPatternSource(w, h, fps int, push func(CapturedFrame)) (*PatternSource, error) {
	if w <= 0 || h <= 0 || fps <= 0 {
		return nil, errors.New("media: bad pattern size")
	}
	s := &PatternSource{w: w, h: h, fps: fps, stop: make(chan struct{}), done: make(chan struct{})}
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
			pb := NewBGRA(w, h)
			if pb == nil {
				continue
			}
			pb.drawPattern(n)
			push(CapturedFrame{Picture: pb, CaptureUS: uint64(time.Now().UnixMicro())})
			pb.Release()
		}
	}()
	return s, nil
}

func (s *PatternSource) Size() (int, int) { return s.w, s.h }
func (s *PatternSource) FPS() int         { return s.fps }
func (s *PatternSource) Stop() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

// NewBGRA allocates an IOSurface-backed BGRA pixel buffer (refcount 1).
func NewBGRA(w, h int) *PixelBuffer {
	ref := C.qv_pb_new_bgra(C.int(w), C.int(h))
	if ref == nil {
		return nil
	}
	return wrapPB(ref)
}

var bars = [8][3]byte{{235, 235, 235}, {235, 235, 16}, {16, 235, 235}, {16, 235, 16}, {235, 16, 235}, {235, 16, 16}, {16, 16, 235}, {16, 16, 16}}

func (p *PixelBuffer) drawPattern(n int) {
	pix, stride, unlock := p.lock(false)
	defer unlock()
	w, h := p.Size()
	box := (n * 8) % max(w-h/4, 1)
	for y := range h {
		row := pix[y*stride:]
		for x := range w {
			c := bars[x*8/w]
			if y > h*3/4 { // bottom quarter: grey with a moving box
				c = [3]byte{40, 40, 40}
				if x >= box && x < box+h/4 {
					c = [3]byte{255, 160, 0}
				}
			}
			// Frame counter as 16 bits across the top strip.
			if y < h/20 {
				bit := x * 16 / w
				if n>>(15-bit)&1 == 1 {
					c = [3]byte{255, 255, 255}
				} else {
					c = [3]byte{0, 0, 0}
				}
			}
			o := x * 4
			row[o], row[o+1], row[o+2], row[o+3] = c[2], c[1], c[0], 255
		}
	}
}
