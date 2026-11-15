package synth

import (
	"testing"

	"github.com/boson-chat/quicvideo/gen/controlpb"
)

func TestParseLayers(t *testing.T) {
	ls, err := ParseLayers("320x180@15:200k, 1280x720@30:1.5m")
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 || ls[1].GetId() != 1 || ls[1].GetWidth() != 1280 || ls[1].GetFps() != 30 || ls[1].GetBitrate() != 1_500_000 {
		t.Fatalf("%v", ls)
	}
	for _, bad := range []string{"320x180", "320x180@15", "axb@1:1k", "320x180@0:1k", "320x180@15:-1"} {
		if _, err := ParseLayers(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestSourceBitrate(t *testing.T) {
	s := NewSource(&controlpb.Layer{Fps: 30, Bitrate: 1_200_000}, 60)
	total, keys := 0, 0
	for range 600 {
		n, k := s.Next()
		total += n
		if k {
			keys++
		}
	}
	// 20 s of video at 1.2 Mbps = 3 MB.
	if total < 2_900_000 || total > 3_100_000 || keys != 10 {
		t.Fatalf("total %d keys %d", total, keys)
	}
	s.ForceKeyframe()
	if _, k := s.Next(); !k {
		t.Fatal("forced keyframe")
	}
}
