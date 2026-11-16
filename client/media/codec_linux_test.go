//go:build linux && cgo

package media

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func roundTrip(t *testing.T, backend int) {
	var mu sync.Mutex
	var frames []EncodedFrame
	enc, err := NewAVEncoder(EncoderConfig{Width: 640, Height: 360, FPS: 30, Bitrate: 800_000, GOP: time.Second}, backend,
		func(f EncodedFrame) {
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
		})
	if err != nil {
		t.Skipf("backend %d not available here: %v", backend, err)
	}
	t.Logf("encoder: %s", enc.Backend())
	for i := range 45 {
		f := newPatternFrame(1280, 720, i) // the encoder scales to the layer size
		if err := enc.Encode(f, uint64(1_000_000+i*33_333), i == 20); err != nil {
			t.Fatal(err)
		}
		f.Release()
	}
	enc.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 40 {
		t.Fatalf("%d frames out", len(frames))
	}
	keys := 0
	for i, f := range frames {
		a := AnnexBToAVCC(f.Data)
		if f.Keyframe {
			keys++
			if a.SPS == nil || a.PPS == nil || !a.Keyframe {
				t.Fatalf("keyframe %d lacks SPS/PPS/IDR", i)
			}
		}
	}
	if !frames[0].Keyframe || keys < 2 {
		t.Fatalf("keyframes: first=%v total=%d", frames[0].Keyframe, keys)
	}
	forced := false
	for _, f := range frames {
		if f.Keyframe && f.CaptureUS == uint64(1_000_000+20*33_333) {
			forced = true
		}
	}
	if !forced {
		t.Fatal("the forced keyframe (frame 20) is missing")
	}

	var decoded []Picture
	dec, err := NewVideoDecoder(func(p Picture, _ uint64) {
		p.Retain()
		decoded = append(decoded, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for i, f := range frames {
		if err := dec.Decode(f.Data, f.CaptureUS); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
	}
	if len(decoded) < len(frames)-2 {
		t.Fatalf("decoded %d of %d", len(decoded), len(frames))
	}
	last := decoded[len(decoded)-1]
	if w, h := last.Size(); w != 640 || h != 360 {
		t.Fatalf("decoded %dx%d", w, h)
	}
	img, err := ToRGBA(last, 0)
	if err != nil {
		t.Fatal(err)
	}
	yr, yg, yb, _ := img.At(640/8+40, 150).RGBA()
	br, bg, bb, _ := img.At(640*6/8+40, 150).RGBA()
	if yr>>8 < 170 || yg>>8 < 170 || yb>>8 > 90 || bb>>8 < 170 || br>>8 > 90 || bg>>8 > 90 {
		t.Fatalf("colours off: yellow (%d,%d,%d) blue (%d,%d,%d)", yr>>8, yg>>8, yb>>8, br>>8, bg>>8, bb>>8)
	}
	for _, p := range decoded {
		p.Release()
	}
}

func TestOpenH264RoundTrip(t *testing.T) { roundTrip(t, BackendOpenH264) }
func TestNVENCRoundTrip(t *testing.T)    { roundTrip(t, BackendNVENC) }
func TestVAAPIRoundTrip(t *testing.T)    { roundTrip(t, BackendVAAPI) }

func TestAutoEncoderNeverPicksGPLByDefault(t *testing.T) {
	t.Setenv("QUICVIDEO_ALLOW_GPL", "")
	enc, err := NewAVEncoder(EncoderConfig{Width: 320, Height: 180, FPS: 15, Bitrate: 200_000, GOP: time.Second}, BackendAuto,
		func(EncodedFrame) {})
	if err != nil {
		t.Skipf("no encoder: %v", err)
	}
	defer enc.Close()
	if strings.Contains(enc.Backend(), "x264") {
		t.Fatalf("auto picked %s without QUICVIDEO_ALLOW_GPL", enc.Backend())
	}
	t.Logf("auto encoder: %s", enc.Backend())
}

func TestPatternSourceLinux(t *testing.T) {
	var mu sync.Mutex
	n := 0
	s, err := NewPatternSource(320, 180, 30, func(f CapturedFrame) {
		if img, err := ToRGBA(f.Picture, 160); err != nil || img.Bounds().Dx() != 160 {
			t.Errorf("ToRGBA: %v", err)
		}
		mu.Lock()
		n++
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	s.Stop()
	if n < 5 {
		t.Fatalf("%d frames", n)
	}
}

// An encoder opened and closed without a frame (a layer switched on and
// straight off) must close cleanly on every backend; h264_vaapi crashed
// flushing one that never encoded.
func TestEncoderCloseWithoutFrames(t *testing.T) {
	for name, b := range map[string]int{"auto": BackendAuto, "openh264": BackendOpenH264, "vaapi": BackendVAAPI, "nvenc": BackendNVENC} {
		t.Run(name, func(t *testing.T) {
			enc, err := NewAVEncoder(EncoderConfig{Width: 640, Height: 360, FPS: 30, Bitrate: 600_000, GOP: time.Second}, b,
				func(EncodedFrame) {})
			if err != nil {
				t.Skipf("no encoder: %v", err)
			}
			enc.Close()
			enc.Close() // and twice
		})
	}
}
