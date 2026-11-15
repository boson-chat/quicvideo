//go:build darwin

package media

import (
	"sync"
	"testing"
	"time"
)

// Real VideoToolbox round trip (runs on macOS only; no permissions needed):
// test pattern → hardware H.264 at two layer sizes → decode → compare.
func TestVideoToolboxRoundTrip(t *testing.T) {
	type out struct {
		data []byte
		key  bool
	}
	var mu sync.Mutex
	var frames []out
	enc, err := NewVTEncoder(EncoderConfig{Width: 640, Height: 360, FPS: 30, Bitrate: 800_000, GOP: time.Second},
		func(f EncodedFrame) {
			mu.Lock()
			frames = append(frames, out{f.Data, f.Keyframe})
			mu.Unlock()
		})
	if err != nil {
		t.Skipf("no VideoToolbox encoder: %v", err)
	}
	t.Logf("hardware encoder: %v", enc.(*VTEncoder).Hardware())
	// Source is 1280×720: the encoder scales to the layer size.
	for i := range 45 {
		pb := NewBGRA(1280, 720)
		pb.drawPattern(i)
		force := i == 20
		if err := enc.Encode(pb, uint64(1_000_000+i*33_333), force); err != nil {
			t.Fatal(err)
		}
		pb.Release()
	}
	enc.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 40 {
		t.Fatalf("only %d frames out", len(frames))
	}
	keys := 0
	for i, f := range frames {
		if f.key {
			keys++
			a := AnnexBToAVCC(f.data)
			if a.SPS == nil || a.PPS == nil || !a.Keyframe {
				t.Fatalf("keyframe %d lacks parameter sets/IDR", i)
			}
		}
	}
	if !frames[0].key || keys < 2 || !frames[20].key {
		t.Fatalf("keyframes: first=%v forced(20)=%v total=%d", frames[0].key, frames[20].key, keys)
	}

	var decoded []*PixelBuffer
	dec, _ := NewDecoder(func(pb *PixelBuffer, _ uint64) {
		pb.Retain()
		decoded = append(decoded, pb)
	})
	defer dec.Close()
	if err := dec.Decode(frames[1].data, 0); err != ErrNeedKeyframe && !frames[1].key {
		t.Fatalf("decoding a delta first: want ErrNeedKeyframe, got %v", err)
	}
	for i, f := range frames {
		if err := dec.Decode(f.data, uint64(i)); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
	}
	if len(decoded) != len(frames) {
		t.Fatalf("decoded %d of %d", len(decoded), len(frames))
	}
	last := decoded[len(decoded)-1]
	if w, h := last.Size(); w != 640 || h != 360 {
		t.Fatalf("decoded size %dx%d", w, h)
	}
	img, err := last.RGBA()
	if err != nil {
		t.Fatal(err)
	}
	// Colour bars survive: the second bar is yellow, the seventh blue.
	y := img.At(640*1/8+40, 150)
	b := img.At(640*6/8+40, 150)
	yr, yg, yb, _ := y.RGBA()
	br, bg, bb, _ := b.RGBA()
	if yr>>8 < 180 || yg>>8 < 180 || yb>>8 > 80 || bb>>8 < 180 || br>>8 > 80 || bg>>8 > 80 {
		t.Fatalf("colours off: yellow %v blue %v", y, b)
	}
	for _, pb := range decoded {
		pb.Release()
	}

	// A smaller layer's keyframe mid-stream: the decoder follows the size change.
	enc2, err := NewVTEncoder(EncoderConfig{Width: 320, Height: 180, FPS: 15, Bitrate: 200_000, GOP: time.Second},
		func(f EncodedFrame) {
			if err := dec.Decode(f.Data, 0); err != nil {
				t.Errorf("decode after switch: %v", err)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	decoded = nil
	pb := NewBGRA(1280, 720)
	pb.drawPattern(0)
	_ = enc2.Encode(pb, 5_000_000, true)
	pb.Release()
	enc2.Close()
	if len(decoded) != 1 {
		t.Fatalf("decoded %d after switch", len(decoded))
	}
	if w, h := decoded[0].Size(); w != 320 || h != 180 {
		t.Fatalf("after switch %dx%d", w, h)
	}
	decoded[0].Release()
}

func TestPatternSource(t *testing.T) {
	var mu sync.Mutex
	n := 0
	s, err := NewPatternSource(320, 180, 30, func(f CapturedFrame) {
		if w, h := f.Picture.Size(); w != 320 || h != 180 {
			t.Errorf("size %dx%d", w, h)
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
	mu.Lock()
	defer mu.Unlock()
	if n < 5 {
		t.Fatalf("%d frames in 300 ms", n)
	}
}
