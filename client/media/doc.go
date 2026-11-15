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
	Stop()
}
