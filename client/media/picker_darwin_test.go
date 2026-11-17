//go:build darwin && !ios

package media

import (
	"bytes"
	"errors"
	"image/jpeg"
	"testing"
)

// The picker's live thumbnail: a small JPEG of a display (macOS 14+, with
// the Screen Recording permission).
func TestScreenThumbnail(t *testing.T) {
	if ScreenPermission() != PermissionGranted {
		t.Skip("no Screen Recording permission here")
	}
	ds, _, err := ListScreens()
	if err != nil || len(ds) == 0 {
		t.Skipf("no displays: %v", err)
	}
	b, err := ScreenThumbnail(ds[0].ID, 0, 320)
	if err != nil {
		if errors.Is(err, ErrScreenUnsupported) || bytes.Contains([]byte(err.Error()), []byte("macOS 14")) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	if cfg.Width > 320 || cfg.Width < 100 {
		t.Fatalf("thumbnail %dx%d", cfg.Width, cfg.Height)
	}
	t.Logf("display %d thumbnail: %dx%d, %d bytes, has self %v", ds[0].ID, cfg.Width, cfg.Height, len(b), ds[0].HasSelf)
}

func TestAppIcon(t *testing.T) {
	b, err := AppIcon("com.apple.finder", 32)
	if err != nil {
		t.Skip(err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Fatalf("not a PNG: % x", b[:8])
	}
	if _, err := AppIcon("no.such.app", 32); err == nil {
		t.Fatal("an icon for an app that doesn't exist")
	}
}
