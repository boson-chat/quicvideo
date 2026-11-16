//go:build android && cgo

package media

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func testNV12(w, h, frame int) *NV12 {
	p := &NV12{W: w, H: h, Data: make([]byte, w*h*3/2)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p.Data[y*w+x] = byte((x + y + frame*4) & 0xff)
		}
	}
	for i := w * h; i < len(p.Data); i++ {
		p.Data[i] = 128
	}
	return p
}

// AMediaCodec encodes NV12 to Annex-B H.264: SPS/PPS before every
// keyframe, and a keyframe on request.
func TestAEncoder(t *testing.T) {
	var mu sync.Mutex
	var frames []EncodedFrame
	enc, err := NewAEncoder(EncoderConfig{Width: 640, Height: 360, FPS: 30, Bitrate: 800_000, GOP: 2 * time.Second},
		func(f EncodedFrame) { mu.Lock(); frames = append(frames, f); mu.Unlock() })
	if err != nil {
		t.Skipf("no encoder here: %v", err)
	}
	defer enc.Close()
	for i := range 60 {
		if err := enc.Encode(testNV12(1280, 720, i), uint64(1_000_000+i*33_333), i == 40); err != nil {
			t.Logf("frame %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	enc.Encode(testNV12(1280, 720, 61), 3_000_000, false)
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 30 {
		t.Fatalf("%d frames out of 60", len(frames))
	}
	if !frames[0].Keyframe {
		t.Fatal("first frame isn't a keyframe")
	}
	keys := 0
	for i, f := range frames {
		if !bytes.HasPrefix(f.Data, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(f.Data, []byte{0, 0, 1}) {
			t.Fatalf("frame %d isn't Annex B: % x", i, f.Data[:8])
		}
		if f.Keyframe {
			keys++
			// SPS (NAL type 7) first.
			if f.Data[4]&0x1f != 7 && f.Data[3]&0x1f != 7 {
				t.Fatalf("keyframe %d doesn't start with SPS: % x", i, f.Data[:8])
			}
		}
	}
	if keys < 2 {
		t.Fatalf("%d keyframes: the requested one didn't come", keys)
	}
	t.Logf("%d frames, %d keyframes, first %d bytes", len(frames), keys, len(frames[0].Data))
}

func TestRotateAndScale(t *testing.T) {
	// 4x2 luma 0..7; rotate 90 clockwise → 2x4: first row = left column bottom-up.
	w, h := 4, 2
	y := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	uv := []byte{10, 11}
	u := []byte{10, 11}
	v := []byte{20, 21}
	_ = uv
	p := yuvToNV12Rotated(y, w, u, v, 2, 1, w, h, 90)
	if p.W != 2 || p.H != 4 {
		t.Fatalf("size %dx%d", p.W, p.H)
	}
	want := []byte{4, 0, 5, 1, 6, 2, 7, 3}
	if !bytes.Equal(p.Data[:8], want) {
		t.Fatalf("luma % d, want % d", p.Data[:8], want)
	}
	s := scaleNV12(testNV12(1280, 720, 0), 640, 360)
	if s.W != 640 || s.H != 360 || len(s.Data) != 640*360*3/2 {
		t.Fatalf("scaled %dx%d %d", s.W, s.H, len(s.Data))
	}
	if img, err := ToRGBA(s, 320); err != nil || img.Bounds().Dx() != 320 {
		t.Fatalf("ToRGBA %v", err)
	}
}
