package media

import (
	"math"
	"testing"
)

func TestAudioResampler(t *testing.T) {
	var got []float32
	var times []int64
	a := &audioResampler{out: func(p []float32, us int64) { got = append(got, p...); times = append(times, us) }}
	// 48 kHz passes through untouched.
	a.push([]float32{0.1, 0.2}, 48000, 7)
	if len(got) != 2 || got[1] != 0.2 || times[0] != 7 {
		t.Fatalf("48 kHz: %v %v", got, times)
	}
	// 24 kHz → twice the samples, a ramp stays a ramp, across buffers.
	got, times = nil, nil
	in := make([]float32, 240)
	for i := range in {
		in[i] = float32(i) / 1000
	}
	a.push(in[:120], 24000, 1)
	a.push(in[120:], 24000, 2)
	if n := len(got); n < 478 || n > 482 {
		t.Fatalf("24 kHz → %d samples, want ~480", n)
	}
	for i := 1; i < len(got); i++ {
		if d := got[i] - got[i-1]; math.Abs(float64(d)-0.0005) > 1e-6 {
			t.Fatalf("not a smooth ramp at %d: %v → %v", i, got[i-1], got[i])
		}
	}
}
