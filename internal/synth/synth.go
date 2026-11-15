// Package synth makes synthetic simulcast video for the example bots and the
// load test: frames with realistic sizes (big keyframes, small deltas) at a
// layer's bitrate and fps, without an encoder.
package synth

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/boson-chat/quicvideo/gen/controlpb"
)

// ParseLayers parses "320x180@15:200k,1280x720@30:1.5m" into layers 0..n-1.
func ParseLayers(s string) ([]*controlpb.Layer, error) {
	var out []*controlpb.Layer
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		res, rest, ok := strings.Cut(part, "@")
		if !ok {
			return nil, fmt.Errorf("layer %q: want WxH@fps:bitrate", part)
		}
		fpsS, brS, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("layer %q: want WxH@fps:bitrate", part)
		}
		wS, hS, ok := strings.Cut(res, "x")
		if !ok {
			return nil, fmt.Errorf("layer %q: bad resolution", part)
		}
		w, err1 := strconv.Atoi(wS)
		h, err2 := strconv.Atoi(hS)
		fps, err3 := strconv.Atoi(fpsS)
		br, err4 := ParseBitrate(brS)
		if err := firstErr(err1, err2, err3, err4); err != nil || fps <= 0 {
			return nil, fmt.Errorf("layer %q: %v", part, err)
		}
		out = append(out, &controlpb.Layer{Id: uint32(i), Width: uint32(w), Height: uint32(h), Fps: uint32(fps), Bitrate: br})
	}
	return out, nil
}

// ParseBitrate parses "200k", "1.5m", "800000".
func ParseBitrate(s string) (uint64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1e3, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1e6, strings.TrimSuffix(s, "m")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("bad bitrate %q", s)
	}
	return uint64(f * mult), nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Source produces frame sizes for one layer. Keyframes are KeyRatio times
// a delta frame; the average over a GOP matches the bitrate.
type Source struct {
	GOPFrames int
	keySize   int
	deltaSize int
	n         int
	force     bool
}

const KeyRatio = 8

func NewSource(l *controlpb.Layer, gopFrames int) *Source {
	if gopFrames < 1 {
		gopFrames = 1
	}
	avg := float64(l.GetBitrate()) / 8 / float64(l.GetFps())
	delta := avg * float64(gopFrames) / float64(gopFrames-1+KeyRatio)
	return &Source{GOPFrames: gopFrames, keySize: int(delta * KeyRatio), deltaSize: max(int(delta), 1)}
}

// ForceKeyframe makes the next frame a keyframe (a keyframe request).
func (s *Source) ForceKeyframe() { s.force = true }

// Next returns the next frame's size and whether it is a keyframe.
func (s *Source) Next() (size int, key bool) {
	if s.force || s.n%s.GOPFrames == 0 {
		s.force = false
		s.n = 1
		return s.keySize, true
	}
	s.n++
	return s.deltaSize, false
}

// Payload returns n bytes from a shared buffer (contents don't matter).
func Payload(n int) []byte {
	if n > len(junk) {
		junk = make([]byte, n*2)
		for i := range junk {
			junk[i] = byte(i * 31)
		}
	}
	return junk[:n]
}

var junk = make([]byte, 1<<20)
