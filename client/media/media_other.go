//go:build !darwin && !(linux && cgo)

package media

import (
	"image"
	"time"
)

// Capture and codecs are macOS-only for now (Windows and Linux are the
// next phase). These keep the API building everywhere.

func ScreenPermission() Permission                     { return PermissionDenied }
func RequestScreenPermission()                         {}
func CameraPermission() Permission                     { return PermissionDenied }
func RequestCameraPermission(time.Duration) Permission { return PermissionDenied }
func ListScreens() ([]Display, []Window, error)        { return nil, nil, ErrUnsupported }
func ListCameras() ([]Camera, error)                   { return nil, ErrUnsupported }

func StartScreen(ScreenOptions, func(CapturedFrame)) (Source, error) { return nil, ErrUnsupported }
func StartCamera(CameraOptions, func(CapturedFrame)) (Source, error) { return nil, ErrUnsupported }

// DefaultEncoder is the platform's hardware encoder factory.
var DefaultEncoder EncoderFactory = func(EncoderConfig, func(EncodedFrame)) (Encoder, error) {
	return nil, ErrUnsupported
}

func NewVideoDecoder(func(Picture, uint64)) (VideoDecoder, error) { return nil, ErrUnsupported }
func ToRGBA(Picture, int) (*image.RGBA, error)                    { return nil, ErrUnsupported }

// NewPatternSource is macOS-only for now.
func NewPatternSource(int, int, int, func(CapturedFrame)) (Source, error) { return nil, ErrUnsupported }
