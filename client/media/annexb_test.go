package media

import (
	"bytes"
	"testing"
)

func TestAVCCRoundTrip(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xC0, 0x1E}
	pps := []byte{0x68, 0xCE, 0x3C, 0x80}
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0x00, 0x03, 0x01, 0x02} // 00 00 03: emulation prevention
	sei := []byte{0x06, 0x05, 0x01}
	var avcc []byte
	for _, n := range [][]byte{sei, idr} {
		avcc = append(avcc, 0, 0, 0, byte(len(n)))
		avcc = append(avcc, n...)
	}
	ab, err := AVCCToAnnexB(nil, avcc, 4, [][]byte{sps, pps}, true)
	if err != nil {
		t.Fatal(err)
	}
	nals := SplitAnnexB(ab)
	if len(nals) != 5 || nals[0][0]&0x1f != nalAUD || !bytes.Equal(nals[1], sps) || !bytes.Equal(nals[2], pps) {
		t.Fatalf("nals %x", nals)
	}
	got := AnnexBToAVCC(ab)
	if !got.Keyframe || !bytes.Equal(got.SPS, sps) || !bytes.Equal(got.PPS, pps) || !bytes.Equal(got.AVCC, avcc) {
		t.Fatalf("round trip: %+v", got)
	}

	// Delta frame: no parameter sets; 2-byte length prefixes work too.
	p := []byte{0x41, 0x9A, 0x00}
	ab, err = AVCCToAnnexB(nil, []byte{0, 3, 0x41, 0x9A, 0x00}, 2, [][]byte{sps, pps}, false)
	if err != nil {
		t.Fatal(err)
	}
	got = AnnexBToAVCC(ab)
	if got.Keyframe || got.SPS != nil || !bytes.Equal(got.AVCC, append([]byte{0, 0, 0, 3}, p...)) {
		t.Fatalf("delta: %+v", got)
	}
}

func TestAVCCMalformed(t *testing.T) {
	for _, bad := range [][]byte{{0, 0, 0}, {0, 0, 0, 9, 1, 2}, {0, 0, 0, 0}} {
		if _, err := AVCCToAnnexB(nil, bad, 4, nil, false); err != ErrBadAVCC {
			t.Errorf("%x: want ErrBadAVCC, got %v", bad, err)
		}
	}
}

func TestSplitAnnexB3ByteCodes(t *testing.T) {
	b := []byte{0, 0, 1, 0x09, 0xF0, 0, 0, 1, 0x41, 1, 2, 0, 0, 0, 1, 0x41, 3}
	nals := SplitAnnexB(b)
	if len(nals) != 3 || !bytes.Equal(nals[1], []byte{0x41, 1, 2}) || !bytes.Equal(nals[2], []byte{0x41, 3}) {
		t.Fatalf("%x", nals)
	}
}
