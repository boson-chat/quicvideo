// Package media captures, encodes and decodes video for the quicvideo SDK.
//
// The pure-Go parts (layer presets, tile → layer mapping, Annex-B/AVCC
// conversion and the Publisher pipeline that drives one encoder per watched
// layer) build everywhere. Capture and the codecs are platform code: macOS
// uses ScreenCaptureKit, AVFoundation and VideoToolbox (cgo, darwin). On
// other platforms those constructors return ErrUnsupported for now.
package media

import "errors"

// ErrUnsupported: no capture/codec implementation on this platform yet.
var ErrUnsupported = errors.New("media: not supported on this platform yet")

// ErrScreenUnsupported: this OS version can't share the screen (macOS
// before 12.3, which has no ScreenCaptureKit). Cameras still work.
var ErrScreenUnsupported = errors.New("media: screen sharing needs macOS 12.3 or later")

// Picture is a captured or decoded image (on macOS a CVPixelBuffer).
// Retain/Release are reference counts: a Picture handed to a callback is
// only valid during the call unless retained.
type Picture interface {
	Size() (w, h int)
	Retain()
	Release()
}

// CapturedFrame is one picture from a Source.
type CapturedFrame struct {
	Picture   Picture
	CaptureUS uint64 // wall clock µs
}

// Source is a running capture (screen, window or camera).
type Source interface {
	// Size is the capture size in pixels (0,0 until the first frame).
	Size() (w, h int)
	// FPS is the capture rate cap.
	FPS() int
	// Stop ends the capture and releases the device. After it returns no
	// more frames are pushed. Safe to call more than once.
	Stop()
}

// Ender is implemented by sources that can end on their own: the camera
// was unplugged or failed, the OS interrupted capture, the user stopped the
// share from the system UI. Ended delivers the reason once. It doesn't fire
// for Stop. The source still needs Stop to release everything.
type Ender interface {
	Ended() <-chan error
}

// ended is a one-shot Ended channel for sources.
type ended struct{ ch chan error }

func newEnded() ended { return ended{ch: make(chan error, 1)} }

func (e ended) Ended() <-chan error { return e.ch }

// end reports the first reason; later ones are dropped.
func (e ended) end(err error) {
	if err == nil {
		err = errors.New("media: capture ended")
	}
	select {
	case e.ch <- err:
	default:
	}
}
