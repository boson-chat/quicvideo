// screenbench measures the sender side of a screen share on this machine:
// capture (ScreenCaptureKit) → the Publisher → the platform encoder, with
// every layer of the screen ladder active. For each stage it reports the
// frame rate and the gaps between frames (p50/p95/max), and per layer the
// encode latency, bitrate and keyframe sizes. Something has to move on the
// screen (scroll a page, play a video) for the numbers to mean anything:
// a still screen sends nothing.
//
//	go run ./examples/screenbench -window Chromium -duration 20s
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/client/media"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

type stage struct {
	name  string
	times []time.Time
	lat   []time.Duration
	bytes int
	keys  []int
}

func (s *stage) add(t time.Time, lat time.Duration, n int, key bool) {
	s.times = append(s.times, t)
	s.lat = append(s.lat, lat)
	s.bytes += n
	if key {
		s.keys = append(s.keys, n)
	}
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, int(float64(len(s)-1)*p+0.5))]
}

func (s *stage) report(from, to time.Time) string {
	var in []time.Time
	var lat []time.Duration
	for i, t := range s.times {
		if !t.Before(from) && t.Before(to) {
			in = append(in, t)
			lat = append(lat, s.lat[i])
		}
	}
	var gaps []time.Duration
	for i := 1; i < len(in); i++ {
		gaps = append(gaps, in[i].Sub(in[i-1]))
	}
	secs := to.Sub(from).Seconds()
	out := fmt.Sprintf("| %-22s | %5.1f | %4d | %4d | %4d |", s.name, float64(len(in))/secs,
		pct(gaps, 0.5).Milliseconds(), pct(gaps, 0.95).Milliseconds(), pct(gaps, 1).Milliseconds())
	if s.lat[0] >= 0 && len(lat) > 0 {
		out += fmt.Sprintf(" %4d | %4d | %6.0f | %d keys, max %d KB |", pct(lat, 0.5).Milliseconds(), pct(lat, 0.95).Milliseconds(),
			float64(s.bytes)*8/1000/s.durationSecs(), len(s.keys), maxInt(s.keys)/1024)
	} else {
		out += "      |      |        | |"
	}
	return out
}

func (s *stage) durationSecs() float64 {
	if len(s.times) < 2 {
		return 1
	}
	return s.times[len(s.times)-1].Sub(s.times[0]).Seconds()
}

func maxInt(v []int) int {
	m := 0
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

// layerWriter records what each layer's encoder puts out (in place of a
// published track).
type layerWriter struct {
	mu     sync.Mutex
	layers map[uint32]*stage
}

func (w *layerWriter) WriteFrame(layer uint32, key bool, captureUS uint64, payload []byte) error {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.layers[layer].add(now, now.Sub(time.UnixMicro(int64(captureUS))), len(payload), key)
	return nil
}

func main() {
	window := flag.String("window", "", "capture the first window whose app or title contains this (default: the main display)")
	duration := flag.Duration("duration", 20*time.Second, "how long")
	warmup := flag.Duration("warmup", 3*time.Second, "not counted")
	fps := flag.Int("fps", 0, "capture rate cap (0: the ladder's top rate)")
	label := flag.String("label", "", "a name for this run")
	addr := flag.String("addr", "", "also publish through this quicvideod (AUTH_INSECURE) and receive it back")
	recvLayer := flag.Int("recv-layer", -1, "with -addr: the receiver's max layer (-1: the top)")
	traceOut := flag.String("trace", "", "with -addr: write the received frames (arrival µs, capture µs, keyframe) here as JSON")
	flag.Parse()

	var o media.ScreenOptions
	if *window != "" {
		_, wins, err := media.ListScreens()
		if err != nil {
			log.Fatal(err)
		}
		for _, w := range wins {
			if strings.Contains(w.App+" "+w.Title, *window) && w.Width > 200 {
				o.WindowID = w.ID
				log.Printf("window %d: %s — %s (%dx%d)", w.ID, w.App, w.Title, w.Width, w.Height)
				break
			}
		}
		if o.WindowID == 0 {
			log.Fatalf("no window matching %q", *window)
		}
	}
	capture := &stage{name: "capture"}
	var mu sync.Mutex
	var pub *media.Publisher
	first := make(chan [2]int, 1)
	push := func(f media.CapturedFrame) {
		now := time.Now()
		mu.Lock()
		capture.add(now, now.Sub(time.UnixMicro(int64(f.CaptureUS))), 0, false)
		p := pub
		mu.Unlock()
		if p != nil {
			p.Push(f)
			return
		}
		w, h := f.Picture.Size()
		select {
		case first <- [2]int{w, h}:
		default:
		}
	}
	// Capture at the top layer's rate, as the boson app does.
	probe := media.ScreenLayers(1920, 1080)
	o.FPS = *fps
	if o.FPS == 0 {
		o.FPS = int(probe[len(probe)-1].GetFps())
	}
	src, err := media.StartScreen(o, push)
	if err != nil {
		log.Fatal(err)
	}
	defer src.Stop()
	var wh [2]int
	select {
	case wh = <-first:
	case <-time.After(10 * time.Second):
		log.Fatal("no picture in 10 s (nothing moving? permission?)")
	}
	layers := media.ScreenLayers(wh[0], wh[1])
	w := &layerWriter{layers: map[uint32]*stage{}}
	for _, l := range layers {
		w.layers[l.GetId()] = &stage{name: fmt.Sprintf("encoded L%d %dx%d@%d", l.GetId(), l.GetWidth(), l.GetHeight(), l.GetFps())}
	}
	var p *media.Publisher
	recv := &stage{name: "received"}
	type arrival struct {
		At      int64 `json:"at"`
		Capture int64 `json:"capture"`
		Key     bool  `json:"key"`
		Layer   int   `json:"layer"`
	}
	var trace []arrival
	var recvMu sync.Mutex
	var states []string
	if *addr != "" {
		ctx := context.Background()
		tlsc := &tls.Config{InsecureSkipVerify: true}
		pc, err := client.Dial(ctx, *addr, client.Options{Token: "sharer", ClientVersion: "screenbench", TLS: tlsc})
		if err != nil {
			log.Fatal(err)
		}
		defer pc.Close()
		room := fmt.Sprintf("bench-%d", time.Now().UnixNano())
		if _, err := pc.Join(ctx, room); err != nil {
			log.Fatal(err)
		}
		rc, err := client.Dial(ctx, *addr, client.Options{Token: "watcher", ClientVersion: "screenbench", TLS: tlsc,
			OnFrame: func(f client.RemoteFrame) {
				now := time.Now()
				recvMu.Lock()
				recv.add(now, now.Sub(time.UnixMicro(int64(f.CaptureUS))), len(f.Payload), f.Keyframe)
				trace = append(trace, arrival{now.UnixMicro(), int64(f.CaptureUS), f.Keyframe, int(f.Layer)})
				recvMu.Unlock()
			}})
		if err != nil {
			log.Fatal(err)
		}
		defer rc.Close()
		if _, err := rc.Join(ctx, room); err != nil {
			log.Fatal(err)
		}
		p2, track, err := media.PublishTrack(ctx, pc, controlpb.TrackKind_SCREEN, layers, recordingFactory(w), media.PublisherOptions{})
		if err != nil {
			log.Fatal(err)
		}
		defer func() { _ = track.Unpublish() }()
		p = p2
		go func() {
			for m := range rc.Events() {
				if t := m.GetTrackAdded().GetTrack(); t != nil {
					top := uint32(len(t.GetLayers()) - 1)
					if *recvLayer >= 0 {
						top = uint32(*recvLayer)
					}
					_ = rc.Subscribe(t.GetTrackId(), top, 0)
				}
				if st := m.GetSubscriptionState(); st != nil {
					recvMu.Lock()
					states = append(states, fmt.Sprintf("%.1fs layer %d target %d paused %v", time.Since(start0).Seconds(), st.GetLayer(), st.GetTargetLayer(), st.GetPaused()))
					recvMu.Unlock()
				}
			}
		}()
	} else {
		p = media.NewPublisher(w, layers, media.DefaultEncoder, media.PublisherOptions{Screen: true})
		for _, l := range layers {
			p.SetActive(l.GetId(), true)
		}
	}
	mu.Lock()
	pub = p
	mu.Unlock()
	start := time.Now()
	start0 = start
	time.Sleep(*duration)
	end := time.Now()
	p.Close()
	from := start.Add(*warmup)
	fmt.Printf("\n%s: capture %dx%d, cap %d fps, %s\n\n", *label, wh[0], wh[1], o.FPS, end.Sub(from).Round(time.Second))
	fmt.Println("| stage | fps | gap p50 ms | gap p95 ms | gap max ms | latency p50 ms | latency p95 ms | kbit/s | keyframes |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	capture.lat[0] = -1
	fmt.Println(capture.report(from, end))
	ids := make([]int, 0, len(w.layers))
	for id := range w.layers {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		s := w.layers[uint32(id)]
		if len(s.times) == 0 {
			fmt.Printf("| %s | 0 | | | | | | | |\n", s.name)
			continue
		}
		fmt.Println(s.report(from, end))
	}
	if *addr != "" {
		recvMu.Lock()
		if len(recv.times) > 0 {
			fmt.Println(recv.report(from, end))
		}
		fmt.Println()
		for _, s := range states {
			fmt.Println("  subscription:", s)
		}
		if *traceOut != "" {
			b, _ := json.Marshal(trace)
			_ = os.WriteFile(*traceOut, b, 0o644)
		}
		recvMu.Unlock()
	}
	_ = os.Stdout.Sync()
	_ = controlpb.TrackKind_SCREEN
}

var start0 time.Time

// recordingFactory wraps the platform encoder so the encoded stages are
// recorded while frames still go out on the published track.
func recordingFactory(w *layerWriter) media.EncoderFactory {
	return func(cfg media.EncoderConfig, out func(media.EncodedFrame)) (media.Encoder, error) {
		var id uint32
		w.mu.Lock()
		for lid, s := range w.layers {
			if strings.Contains(s.name, fmt.Sprintf(" %dx%d@", cfg.Width, cfg.Height)) {
				id = lid
			}
		}
		w.mu.Unlock()
		return media.DefaultEncoder(cfg, func(f media.EncodedFrame) {
			_ = w.WriteFrame(id, f.Keyframe, f.CaptureUS, f.Data)
			out(f)
		})
	}
}
