package media

import (
	"errors"
	"fmt"
)

// Display is a screen that can be captured (sizes in pixels).
type Display struct {
	ID            uint32
	Width, Height int
	Main          bool
}

// Window is an on-screen window that can be captured.
type Window struct {
	ID            uint32
	Title, App    string
	Width, Height int
}

// Camera is a video capture device.
type Camera struct {
	ID      string // stable unique id
	Name    string
	Formats []CameraFormat
}

// CameraFormat is a size the camera can deliver, with its best frame rate.
type CameraFormat struct {
	Width, Height int
	MaxFPS        int
}

// ScreenOptions select what to capture. Set DisplayID or WindowID (both
// zero = the main display).
type ScreenOptions struct {
	DisplayID uint32
	WindowID  uint32
	// MaxWidth caps the capture width (default 1920, the top screen layer):
	// capturing a 3024-wide Retina panel only to scale it down wastes GPU.
	MaxWidth int
	FPS      int  // capture rate cap (default 15)
	Cursor   bool // draw the cursor
}

// CameraOptions select a camera and format (zero values: default camera,
// 1280×720, 30 fps; the closest supported format is used).
type CameraOptions struct {
	ID            string
	Width, Height int
	FPS           int
}

// Permission is a privacy (TCC) permission state.
type Permission int

const (
	PermissionUndetermined Permission = iota // the system would ask
	PermissionGranted
	PermissionDenied // denied or restricted: only System Settings can change it
)

func (p Permission) String() string {
	return [...]string{"not determined", "granted", "denied"}[p]
}

// PermissionError explains a missing privacy permission and how to grant it.
type PermissionError struct {
	What  string // "Screen Recording" or "Camera"
	State Permission
	Help  string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("media: %s permission %s. %s", e.What, e.State, e.Help)
}

var (
	ErrScreenPermission = errors.New("media: screen recording not permitted")
	ErrCameraPermission = errors.New("media: camera not permitted")
)

func (e *PermissionError) Is(target error) bool {
	return (target == ErrScreenPermission && e.What == "Screen Recording") ||
		(target == ErrCameraPermission && e.What == "Camera")
}

const (
	screenHelp = "Open System Settings → Privacy & Security → Screen & System Audio Recording, " +
		"enable the app that runs this program (the app itself, or your terminal when started from one), " +
		"then quit and reopen it: macOS applies this permission only at launch."
	cameraHelp = "Open System Settings → Privacy & Security → Camera and enable the app that runs this program " +
		"(the app itself, or your terminal when started from one). Bundled apps need NSCameraUsageDescription " +
		"in their Info.plist."
)
