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
	// HasSelf: one of this app's windows is on it. Sharing it shows the
	// call inside the share (the windows themselves are left out).
	HasSelf bool
}

// Window is an on-screen window that can be captured.
type Window struct {
	ID            uint32
	Title, App    string
	BundleID      string // the app's (macOS), for its icon
	Width, Height int
}

// CameraKind is how a camera is attached.
type CameraKind string

const (
	CameraBuiltIn    CameraKind = "builtin"    // e.g. the FaceTime camera
	CameraExternal   CameraKind = "external"   // a physical USB/Thunderbolt webcam
	CameraContinuity CameraKind = "continuity" // an iPhone
	CameraVirtual    CameraKind = "virtual"    // OBS, Immersed, Detail...: software, not a camera
)

// Camera is a video capture device.
type Camera struct {
	ID   string // stable unique id
	Name string
	Kind CameraKind
	// Facing: "front" (the user's side: a phone's selfie camera), "back",
	// or "" (a webcam, external, unknown). Apps mirror their own preview of
	// a front camera only; what's sent is never mirrored.
	Facing  string
	Formats []CameraFormat
	// Unavailable says why the camera can't deliver pictures right now
	// ("" = it can): e.g. a MacBook's built-in camera with the lid closed,
	// which the OS still lists.
	Unavailable string
}

// LidClosedReason is a closed MacBook's built-in camera's Unavailable.
const LidClosedReason = "the built-in camera is off while the lid is closed"

// DefaultCamera picks the camera to use when none is chosen: built-in
// first, then a physical external webcam, then Continuity (iPhone), and a
// virtual camera only if there's nothing else. The OS default is often a
// virtual camera, which is rarely what someone joining a call wants.
// Unavailable cameras (a closed lid's built-in) come last: one is only
// returned when there's nothing else, and starting it fails with its
// reason.
func DefaultCamera(cams []Camera) (Camera, bool) {
	rank := func(k CameraKind) int {
		switch k {
		case CameraBuiltIn:
			return 0
		case CameraExternal:
			return 1
		case CameraContinuity:
			return 2
		}
		return 3
	}
	score := func(c Camera) int {
		if c.Unavailable != "" {
			return 10 + rank(c.Kind)
		}
		return rank(c.Kind)
	}
	best := -1
	for i, c := range cams {
		if best < 0 || score(c) < score(cams[best]) {
			best = i
		}
	}
	if best < 0 {
		return Camera{}, false
	}
	return cams[best], true
}

// CameraFormat is a size the camera can deliver, with its best frame rate.
type CameraFormat struct {
	Width, Height int
	MaxFPS        int
}

// PortalPicker as a ScreenOptions.DisplayID means "let the system's
// screen-share picker choose" (Linux/Wayland: xdg-desktop-portal). Where
// there's no system picker it means the main display.
const PortalPicker = 0xFFFFFFFF

// ScreenOptions select what to capture. Set DisplayID or WindowID (both
// zero = the main display).
type ScreenOptions struct {
	DisplayID uint32
	WindowID  uint32
	// MaxWidth caps the capture width (default 1920, the top screen layer):
	// capturing a 3024-wide Retina panel only to scale it down wastes GPU.
	MaxWidth int
	FPS      int // capture rate cap (default ScreenFPS, the top screen layer's)
	// Mode is what the share is for: ScreenMotion (default: smooth, 30
	// fps) or ScreenDetail (text and detail: sharper, up to 1440p, 15 fps).
	Mode   ScreenMode
	Cursor bool // draw the cursor
}

// CameraOptions select a camera and format (zero values: default camera,
// 1280×720, 30 fps; the closest supported format is used).
type CameraOptions struct {
	ID            string
	Width, Height int
	FPS           int
}

// VideoDecoder decodes one track's Annex-B access units to Pictures.
type VideoDecoder interface {
	Decode(au []byte, captureUS uint64) error
	Close()
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

// ScreenMode: what a screen share is optimized for.
type ScreenMode string

const (
	ScreenMotion ScreenMode = "motion" // video and scrolling: 1080p, 30 fps
	ScreenDetail ScreenMode = "detail" // text and detail: up to 1440p, 15 fps
)

// ScreenCapture is the capture size cap and rate for a mode.
func ScreenCapture(mode ScreenMode) (maxWidth, fps int) {
	if mode == ScreenDetail {
		return 2560, 15
	}
	return 1920, ScreenFPS
}

// screenDefaults fills a zero capture rate and size cap from the mode.
func (o *ScreenOptions) screenDefaults() {
	maxW, fps := ScreenCapture(o.Mode)
	if o.FPS <= 0 {
		o.FPS = fps
	}
	if o.MaxWidth <= 0 {
		o.MaxWidth = maxW
	}
}
