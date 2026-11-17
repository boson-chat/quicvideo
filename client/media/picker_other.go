//go:build !darwin

package media

// AppIcon is macOS-only for now (window shares elsewhere have no icons).
func AppIcon(string, int) ([]byte, error) { return nil, ErrUnsupported }

// ScreenThumbnail is macOS-only for now (Windows: Windows.Graphics.Capture
// thumbnails are a later step; Linux uses the system's portal picker).
func ScreenThumbnail(uint32, uint32, int) ([]byte, error) { return nil, ErrUnsupported }
