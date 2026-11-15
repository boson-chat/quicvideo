// Package trace holds encoder traces: per-frame sizes, timing and keyframe
// flags recorded from a real encoder (examples/vtrace), replayed by the
// bots (examples/pub -trace) so bad-network tests see real encoder output
// without shipping any pictures.
package trace

import (
	"encoding/json"
	"os"

	"github.com/boson-chat/quicvideo/gen/controlpb"
)

// Frame is one encoded frame of one layer.
type Frame struct {
	Layer     int   `json:"l"`
	OffsetUS  int64 `json:"t"`  // capture time since the start of the recording
	Size      int   `json:"s"`  // bytes as encoded in the normal stream
	Key       bool  `json:"k"`  // keyframe in the normal stream
	IntraSize int   `json:"ks"` // bytes of the same picture encoded as a forced keyframe
}

// Trace is a recording.
type Trace struct {
	Source     string             `json:"source"`  // "camera" or "screen"
	Encoder    string             `json:"encoder"` // e.g. "VideoToolbox H.264 (hardware, low-latency)"
	DurationUS int64              `json:"duration_us"`
	Layers     []*controlpb.Layer `json:"layers"`
	Frames     []Frame            `json:"frames"`
}

func Load(path string) (*Trace, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Trace
	return &t, json.Unmarshal(b, &t)
}

func (t *Trace) Save(path string) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ByLayer splits the frames per layer, in time order.
func (t *Trace) ByLayer() [][]Frame {
	out := make([][]Frame, len(t.Layers))
	for _, f := range t.Frames {
		if f.Layer >= 0 && f.Layer < len(out) {
			out[f.Layer] = append(out[f.Layer], f)
		}
	}
	return out
}
