//go:build !darwin

package media

import "time"

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
