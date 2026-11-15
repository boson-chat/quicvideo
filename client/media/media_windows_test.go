//go:build windows && cgo

package media

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNV12ConversionRoundTrip(t *testing.T) {
	const w, h = 64, 32
	src := make([]byte, w*h*4)
	drawPatternBGRA(src, w*4, w, h, 0) // colour bars
	out := bgra2nv12roundtrip(src, w, h, w, h)
	// Yellow bar (2nd) and blue bar (7th), mid-height: within NV12 rounding.
	for _, c := range []struct {
		x       int
		b, g, r byte
	}{{w*1/8 + 3, 16, 235, 235}, {w*6/8 + 3, 235, 16, 16}} {
		o := (h/2*w + c.x) * 4
		for i, want := range []byte{c.b, c.g, c.r} {
			if d := int(out[o+i]) - int(want); d < -24 || d > 24 {
				t.Fatalf("x=%d channel %d: got %d want ~%d", c.x, i, out[o+i], want)
			}
		}
	}
	// Downscaling keeps the bars in place.
	small := bgra2nv12roundtrip(src, w, h, w/2, h/2)
	if b := small[(h/4*(w/2)+w*6/16+1)*4]; b < 200 {
		t.Fatalf("scaled blue bar lost: B=%d", b)
	}
}

func TestMediaFoundationRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var frames []EncodedFrame
	enc, err := NewMFEncoder(EncoderConfig{Width: 640, Height: 360, FPS: 30, Bitrate: 800_000, GOP: time.Second}, true,
		func(f EncodedFrame) {
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
		})
	if err != nil {
		t.Skipf("no H.264 encoder MFT here: %v", err)
	}
	t.Logf("encoder: %s", enc.Backend())
	for i := range 45 {
		p := newPatternPicture(1280, 720, i)
		if err := enc.Encode(p, uint64(1_000_000+i*33_333), i == 20); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		p.Release()
	}
	enc.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 30 {
		t.Fatalf("only %d frames", len(frames))
	}
	if !frames[0].Keyframe || AnnexBToAVCC(frames[0].Data).SPS == nil {
		t.Fatal("first frame must be a keyframe with SPS/PPS")
	}
	var decoded []Picture
	dec, err := NewVideoDecoder(func(p Picture, _ uint64) { p.Retain(); decoded = append(decoded, p) })
	if err != nil {
		t.Skipf("no H.264 decoder MFT: %v", err)
	}
	defer dec.Close()
	for i, f := range frames {
		if err := dec.Decode(f.Data, f.CaptureUS); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
	}
	if len(decoded) < len(frames)/2 {
		t.Fatalf("decoded %d of %d", len(decoded), len(frames))
	}
	last := decoded[len(decoded)-1]
	if w, h := last.Size(); w != 640 || h != 360 {
		t.Fatalf("decoded %dx%d", w, h)
	}
	img, _ := ToRGBA(last, 0)
	if r, g, b, _ := img.At(640/8+40, 150).RGBA(); r>>8 < 170 || g>>8 < 170 || b>>8 > 90 {
		t.Fatalf("yellow bar off: %d %d %d", r>>8, g>>8, b>>8)
	}
	for _, p := range decoded {
		p.Release()
	}
}

func TestListScreensAndCameras(t *testing.T) {
	ds, ws, err := ListScreens()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d displays, %d windows", len(ds), len(ws))
	for _, d := range ds {
		t.Logf("display %d: %dx%d main=%v", d.ID, d.Width, d.Height, d.Main)
	}
	cams, err := ListCameras()
	if err != nil {
		t.Logf("cameras: %v", err)
	}
	for _, c := range cams {
		t.Logf("camera %s (%s): %s", c.Name, c.Kind, c.ID)
	}
}

// A real Windows.Graphics.Capture run of the primary monitor: validates the
// hand-declared WinRT interfaces. Skips where WGC can't run (no desktop).
func TestWindowsGraphicsCapture(t *testing.T) {
	var mu sync.Mutex
	n := 0
	var size [2]int
	src, err := StartScreen(ScreenOptions{FPS: 15}, func(f CapturedFrame) {
		mu.Lock()
		n++
		size[0], size[1] = f.Picture.Size()
		mu.Unlock()
	})
	if err != nil {
		if strings.Contains(err.Error(), "isn't supported") {
			t.Skip(err)
		}
		t.Fatalf("StartScreen: %v", err)
	}
	time.Sleep(2 * time.Second)
	src.Stop()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("WGC: %d frames, %dx%d", n, size[0], size[1])
	if n == 0 {
		t.Skip("no frame in 2 s (static screen or headless session)")
	}
	if size[0] < 320 || size[1] < 200 {
		t.Fatalf("odd capture size %v", size)
	}
}
