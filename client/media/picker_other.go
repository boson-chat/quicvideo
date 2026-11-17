//go:build !darwin

package media

// AppIcon is macOS-only for now (window shares elsewhere have no icons).
func AppIcon(string, int) ([]byte, error) { return nil, ErrUnsupported }

// ScreenAudioSupported: a screen share's sound is macOS-only for now.
func ScreenAudioSupported() bool { return false }
