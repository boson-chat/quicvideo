//go:build windows && cgo

package media

import (
	"bytes"
	"errors"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

// Live picker thumbnails through real Windows.Graphics.Capture: a display
// and a window, as JPEGs at most 320 wide; one capture per source, reused;
// and stopped when the picker closes or nobody asks.
func TestScreenThumbnailWindows(t *testing.T) {
	ds, ws, err := ListScreens()
	if err != nil || len(ds) == 0 {
		t.Skipf("no displays: %v", err)
	}
	b, err := ScreenThumbnail(ds[0].ID, 0, 320)
	if errors.Is(err, ErrUnsupported) || (err != nil && strings.Contains(err.Error(), "isn't supported")) {
		t.Skip(err)
	}
	if errors.Is(err, ErrNoThumbnail) {
		t.Skip("no picture of the display (a headless session)")
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	if cfg.Width > 320 || cfg.Width < 100 || cfg.Height < 50 {
		t.Fatalf("display thumbnail %dx%d", cfg.Width, cfg.Height)
	}
	t.Logf("display %d: %dx%d thumbnail, %d bytes", ds[0].ID, cfg.Width, cfg.Height, len(b))
	// Asked again: the same capture.
	if _, err := ScreenThumbnail(ds[0].ID, 0, 320); err != nil {
		t.Fatal(err)
	}
	if n := thumbCaptures(); n != 1 {
		t.Fatalf("%d captures for one source", n)
	}
	for _, w := range ws {
		wb, err := ScreenThumbnail(0, w.ID, 320)
		if err != nil {
			t.Logf("window %q: %v", w.Title, err) // a minimized window has none
			continue
		}
		if c, err := jpeg.DecodeConfig(bytes.NewReader(wb)); err != nil || c.Width > 320 {
			t.Fatalf("window %q thumbnail: %v %dx%d", w.Title, err, c.Width, c.Height)
		}
		t.Logf("window %q: thumbnail ok", w.Title)
		break
	}
	StopThumbnails()
	if n := thumbCaptures(); n != 0 {
		t.Fatalf("%d captures left after StopThumbnails", n)
	}
	// Nobody asking: the capture stops on its own.
	if _, err := ScreenThumbnail(ds[0].ID, 0, 320); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(thumbIdle + 3*time.Second)
	for thumbCaptures() > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := thumbCaptures(); n != 0 {
		t.Fatalf("%d idle captures still running", n)
	}
}
