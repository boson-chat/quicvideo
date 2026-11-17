//go:build !darwin && !(windows && cgo)

package media

// ScreenThumbnail: macOS and Windows only (Linux uses the system's portal
// picker).
func ScreenThumbnail(uint32, uint32, int) ([]byte, error) { return nil, ErrUnsupported }

// StopThumbnails: nothing to stop here.
func StopThumbnails() {}
