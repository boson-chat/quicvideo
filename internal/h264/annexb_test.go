package h264

import "testing"

func TestSplit(t *testing.T) {
	aud := []byte{0, 0, 0, 1, 0x09, 0xf0}
	sps := []byte{0, 0, 0, 1, 0x67, 1, 2}
	idr := []byte{0, 0, 1, 0x65, 9, 9}
	p := []byte{0, 0, 1, 0x41, 7}
	var data []byte
	for _, part := range [][]byte{aud, sps, idr, aud, p, aud, p} {
		data = append(data, part...)
	}
	aus, err := Split(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(aus) != 3 || !aus[0].Keyframe || aus[1].Keyframe || len(aus[0].Data) != len(aud)+len(sps)+len(idr) {
		t.Fatalf("got %d aus: %+v", len(aus), aus)
	}
	if _, err := Split(append(sps, idr...)); err != ErrNoAUD {
		t.Fatalf("want ErrNoAUD, got %v", err)
	}
}
