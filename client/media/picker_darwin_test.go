//go:build darwin && !ios

package media

import (
	"bytes"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"testing"
	"time"
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

// A display share with its sound (macOS 13+): 48 kHz mono buffers, stamped
// with the capture clock. QV_AUDIO_TEST=1 also plays a system sound and
// checks it's heard (it's audible, so it's off by default).
func TestScreenAudio(t *testing.T) {
	if ScreenPermission() != PermissionGranted {
		t.Skip("no Screen Recording permission here")
	}
	if !ScreenAudioSupported() {
		t.Skip("no screen sound before macOS 13")
	}
	type buf struct {
		n    int
		peak float32
		us   int64
		at   int64
	}
	got := make(chan buf, 1024)
	src, err := StartScreen(ScreenOptions{Audio: func(p []float32, us int64) {
		var peak float32
		for _, v := range p {
			if v < 0 {
				v = -v
			}
			peak = max(peak, v)
		}
		select {
		case got <- buf{len(p), peak, us, time.Now().UnixMicro()}:
		default:
		}
	}}, func(CapturedFrame) {})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Stop()
	if os.Getenv("QV_AUDIO_TEST") == "1" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = exec.Command("afplay", "-v", "0.3", "/System/Library/Sounds/Tink.aiff").Run()
		}()
	}
	deadline := time.After(3 * time.Second)
	samples, loud := 0, false
	var first, last int64
	for done := false; !done; {
		select {
		case b := <-got:
			if first == 0 {
				first = b.us
			}
			last = b.us
			samples += b.n
			loud = loud || b.peak > 0.01
			if lag := b.at - b.us; lag < 0 || lag > 500_000 {
				t.Errorf("a buffer stamped %d µs before it arrived", lag)
			}
		case <-deadline:
			done = true
		}
	}
	if samples == 0 {
		t.Skip("no sound buffers (SCK sends none while nothing plays)")
	}
	// The samples cover about the time they span, at 48 kHz.
	span := float64(last-first) / 1e6
	t.Logf("%d samples over %.2fs of capture time, heard something: %v", samples, span, loud)
	if span > 0.5 {
		if rate := float64(samples) / span; rate < 40000 || rate > 56000 {
			t.Errorf("about %.0f samples/s, want 48000", rate)
		}
	}
	if os.Getenv("QV_AUDIO_TEST") == "1" && !loud {
		t.Error("the system sound wasn't in the share's sound")
	}
}
