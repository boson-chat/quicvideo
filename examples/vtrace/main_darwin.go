//go:build darwin

// vtrace records a real encoder's output as a trace: it captures a camera
// or the screen, runs every simulcast layer through VideoToolbox exactly
// like the Publisher does (same layer ladder, fps caps, GOP, low-latency
// settings), and also encodes each picture as a forced keyframe, so a
// replay can answer keyframe requests with the right size. Only sizes,
// timing and keyframe flags are written: no pictures.
//
//	go run ./examples/vtrace -source screen -duration 60s -out netem/traces/screen.json
//	go run ./examples/vtrace -source camera -camera-id <id> -out netem/traces/camera.json
package main

import (
	"flag"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/client/media"
	"github.com/boson-chat/quicvideo/internal/trace"
)

func main() {
	source := flag.String("source", "screen", "screen | camera")
	cameraID := flag.String("camera-id", "", "camera id (see viewer -list)")
	duration := flag.Duration("duration", 60*time.Second, "recording length")
	out := flag.String("out", "trace.json", "output file")
	flag.Parse()

	var mu sync.Mutex
	var start time.Time
	var src media.Source
	var err error
	frames := make(chan media.CapturedFrame, 8)
	push := func(f media.CapturedFrame) {
		f.Picture.Retain()
		select {
		case frames <- f:
		default:
			f.Picture.Release() // encoder busy: drop, like a real capture would
		}
	}
	if *source == "camera" {
		src, err = media.StartCamera(media.CameraOptions{ID: *cameraID, Width: 1280, Height: 720, FPS: 30}, push)
	} else {
		src, err = media.StartScreen(media.ScreenOptions{FPS: 15, Cursor: true}, push)
	}
	if err != nil {
		log.Fatal(err)
	}
	first := <-frames
	w, h := first.Picture.Size()
	t := &trace.Trace{Source: *source, Encoder: "VideoToolbox H.264 (hardware, low-latency rate control)"}
	gop := 2 * time.Second
	if *source == "screen" {
		t.Layers = media.ScreenLayers(w, h)
		gop = 4 * time.Second
	} else {
		t.Layers = media.CameraLayers(w, h, src.FPS())
	}
	log.Printf("recording %s %dx%d for %v, %d layers", *source, w, h, *duration, len(t.Layers))

	type pending struct {
		size, intra  int
		key          bool
		haveN, haveI bool
	}
	results := make([]map[uint64]*pending, len(t.Layers))
	normal := make([]media.Encoder, len(t.Layers))
	intra := make([]media.Encoder, len(t.Layers))
	for i, l := range t.Layers {
		i := i
		results[i] = map[uint64]*pending{}
		get := func(us uint64) *pending {
			p := results[i][us]
			if p == nil {
				p = &pending{}
				results[i][us] = p
			}
			return p
		}
		cfg := media.EncoderConfig{Width: int(l.GetWidth()), Height: int(l.GetHeight()), FPS: int(l.GetFps()),
			Bitrate: l.GetBitrate(), GOP: gop, Screen: *source == "screen"}
		if normal[i], err = media.NewVTEncoder(cfg, func(f media.EncodedFrame) {
			mu.Lock()
			p := get(f.CaptureUS)
			p.size, p.key, p.haveN = len(f.Data), f.Keyframe, true
			mu.Unlock()
		}); err != nil {
			log.Fatal(err)
		}
		if intra[i], err = media.NewVTEncoder(cfg, func(f media.EncodedFrame) {
			mu.Lock()
			p := get(f.CaptureUS)
			p.intra, p.haveI = len(f.Data), true
			mu.Unlock()
		}); err != nil {
			log.Fatal(err)
		}
	}
	start = time.Now()
	t0 := first.CaptureUS
	last := make([]uint64, len(t.Layers))
	deadline := time.After(*duration)
	encode := func(f media.CapturedFrame) {
		for i, l := range t.Layers {
			interval := uint64(time.Second.Microseconds()) / uint64(l.GetFps())
			if last[i] != 0 && f.CaptureUS < last[i]+interval*85/100 {
				continue
			}
			last[i] = f.CaptureUS
			_ = normal[i].Encode(f.Picture, f.CaptureUS, false)
			_ = intra[i].Encode(f.Picture, f.CaptureUS, true)
		}
		f.Picture.Release()
	}
	encode(first)
loop:
	for {
		select {
		case f := <-frames:
			encode(f)
		case <-deadline:
			break loop
		}
	}
	src.Stop()
	for i := range t.Layers {
		normal[i].Close()
		intra[i].Close()
	}
	t.DurationUS = time.Since(start).Microseconds()
	for i, m := range results {
		for us, p := range m {
			if p.haveN && p.haveI {
				t.Frames = append(t.Frames, trace.Frame{Layer: i, OffsetUS: int64(us - t0), Size: p.size, Key: p.key, IntraSize: p.intra})
			}
		}
	}
	sort.Slice(t.Frames, func(a, b int) bool { return t.Frames[a].OffsetUS < t.Frames[b].OffsetUS })
	if err := t.Save(*out); err != nil {
		log.Fatal(err)
	}
	for i, l := range t.Layers {
		var bytes, n, keys int
		for _, f := range t.Frames {
			if f.Layer == i {
				bytes += f.Size
				n++
				if f.Key {
					keys++
				}
			}
		}
		secs := float64(t.DurationUS) / 1e6
		log.Printf("layer %d %dx%d@%d target %dk: %d frames (%.1f fps), %d keyframes, %.0f kbps actual",
			i, l.GetWidth(), l.GetHeight(), l.GetFps(), l.GetBitrate()/1000, n, float64(n)/secs, keys, float64(bytes)*8/secs/1000)
	}
	log.Printf("wrote %s (%d frames)", *out, len(t.Frames))
}
