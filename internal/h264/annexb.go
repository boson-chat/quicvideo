// Package h264 splits an H.264 Annex-B stream into access units, for the
// example publisher's file mode. The stream must carry access unit
// delimiters (ffmpeg: -bsf:v h264_metadata=aud=insert).
package h264

import (
	"bytes"
	"errors"
)

// AccessUnit is one frame's NAL units, in Annex-B form (start codes kept).
type AccessUnit struct {
	Data     []byte
	Keyframe bool // contains an IDR slice
}

var ErrNoAUD = errors.New("h264: no access unit delimiters (encode with -bsf:v h264_metadata=aud=insert)")

// Split cuts data at each access unit delimiter (NAL type 9).
func Split(data []byte) ([]AccessUnit, error) {
	var starts []int // offsets of start codes of AUD NALs
	var aus []AccessUnit
	nals := nalOffsets(data)
	for _, n := range nals {
		if n.typ == 9 {
			starts = append(starts, n.start)
		}
	}
	if len(starts) == 0 {
		return nil, ErrNoAUD
	}
	starts = append(starts, len(data))
	for i := 0; i+1 < len(starts); i++ {
		au := AccessUnit{Data: data[starts[i]:starts[i+1]]}
		for _, n := range nals {
			if n.start >= starts[i] && n.start < starts[i+1] && n.typ == 5 {
				au.Keyframe = true
			}
		}
		aus = append(aus, au)
	}
	return aus, nil
}

type nal struct {
	start int // offset of the start code
	typ   byte
}

func nalOffsets(data []byte) []nal {
	var out []nal
	i := 0
	for {
		j := bytes.Index(data[i:], []byte{0, 0, 1})
		if j < 0 {
			return out
		}
		sc := i + j
		hdr := sc + 3
		if hdr >= len(data) {
			return out
		}
		start := sc
		if sc > 0 && data[sc-1] == 0 {
			start = sc - 1 // 4-byte start code
		}
		out = append(out, nal{start: start, typ: data[hdr] & 0x1f})
		i = hdr
	}
}
