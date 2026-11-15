package media

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// H.264 NAL unit types used here.
const (
	nalIDR = 5
	nalSEI = 6
	nalSPS = 7
	nalPPS = 8
	nalAUD = 9
)

var ErrBadAVCC = errors.New("media: malformed AVCC sample")

var startCode = []byte{0, 0, 0, 1}

// AVCCToAnnexB converts an encoder's AVCC sample (NALs with lenSize-byte
// big-endian length prefixes) into the Annex-B access unit quicvideo sends:
// an access unit delimiter, then, on keyframes, the parameter sets (SPS,
// PPS), then the sample's NALs, each with a 4-byte start code. Receivers
// can start decoding at any keyframe.
func AVCCToAnnexB(dst, avcc []byte, lenSize int, params [][]byte, keyframe bool) ([]byte, error) {
	dst = append(dst, startCode...)
	dst = append(dst, nalAUD, 0xF0)
	if keyframe {
		for _, p := range params {
			dst = append(dst, startCode...)
			dst = append(dst, p...)
		}
	}
	for len(avcc) > 0 {
		if len(avcc) < lenSize {
			return dst, ErrBadAVCC
		}
		var n int
		for i := range lenSize {
			n = n<<8 | int(avcc[i])
		}
		avcc = avcc[lenSize:]
		if n == 0 || n > len(avcc) {
			return dst, ErrBadAVCC
		}
		dst = append(dst, startCode...)
		dst = append(dst, avcc[:n]...)
		avcc = avcc[n:]
	}
	return dst, nil
}

// SplitAnnexB returns the NAL units of an Annex-B stream (start codes
// removed; 3- and 4-byte start codes both work).
func SplitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i+2 < len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				for end > start && b[end-1] == 0 { // trailing zero of a 4-byte code
					end--
				}
				out = append(out, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// AccessUnit is an Annex-B access unit prepared for a decoder that wants
// AVCC (VideoToolbox): its slice/SEI NALs with 4-byte length prefixes, and
// the parameter sets if it carried any.
type AccessUnit struct {
	AVCC     []byte
	SPS, PPS []byte
	Keyframe bool // contains an IDR slice
}

// AnnexBToAVCC parses an access unit. AUDs are dropped; SPS/PPS are
// returned separately (a decoder builds its format description from them).
func AnnexBToAVCC(au []byte) AccessUnit {
	var out AccessUnit
	var buf bytes.Buffer
	for _, nal := range SplitAnnexB(au) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case nalAUD:
			continue
		case nalSPS:
			out.SPS = nal
			continue
		case nalPPS:
			out.PPS = nal
			continue
		case nalIDR:
			out.Keyframe = true
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(nal)))
		buf.Write(l[:])
		buf.Write(nal)
	}
	out.AVCC = buf.Bytes()
	return out
}
