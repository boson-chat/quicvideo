// netbench is the viewer side of the bad-network tests (netem/): it joins a
// room, subscribes to every track at its top layer (the server picks what
// the link can take), records every frame and SubscriptionState, and prints
// per-track results as markdown table rows (and JSON with -json).
//
// Measured over [warmup, end] (or [event, end] with -event-at):
//
//   - layer: the layer most frames came on (and the range seen)
//   - fps, latency p50/p99 (arrival − capture; sender and receiver must share
//     a clock, as containers on one host do)
//   - freeze: total time beyond a gap of max(250ms, 3 frame intervals)
//     between frames
//   - recovery (with -event-at): time from the event until the track runs
//     steadily again: 2 s in a row with ≥ 70% of its layer's fps and p90
//     latency < 400 ms
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
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

type sample struct {
	at      time.Duration // since start
	layer   uint32
	latency time.Duration
}

type stateChange struct {
	At       float64 `json:"at_s"`
	Layer    uint32  `json:"layer"`
	Target   uint32  `json:"target"`
	Paused   bool    `json:"paused"`
	Estimate uint64  `json:"estimate_kbps"`
}

type trackResult struct {
	Scenario    string        `json:"scenario"`
	Track       string        `json:"track"`
	Layer       string        `json:"layer"`
	FPS         float64       `json:"fps"`
	Kbps        float64       `json:"kbps"`
	P50ms       float64       `json:"latency_p50_ms"`
	P99ms       float64       `json:"latency_p99_ms"`
	FreezeS     float64       `json:"freeze_s"`
	RecoveryS   *float64      `json:"recovery_s,omitempty"`
	Cuts        int           `json:"groups_cut"`
	States      []stateChange `json:"states"`
	Unrecovered bool          `json:"unrecovered,omitempty"`
}

func main() {
	addr := flag.String("addr", "localhost:7710", "server")
	token := flag.String("token", "viewer", "token")
	room := flag.String("room", "call", "room")
	insecure := flag.Bool("insecure", false, "skip TLS verification")
	duration := flag.Duration("duration", 30*time.Second, "run time")
	warmup := flag.Duration("warmup", 10*time.Second, "excluded from steady-state results")
	eventAt := flag.Duration("event-at", 0, "time of a network change (measure from here, report recovery)")
	scenario := flag.String("scenario", "baseline", "name for the results")
	jsonOut := flag.String("json", "", "write results JSON here")
	flag.Parse()

	start := time.Now()
	var mu sync.Mutex
	samples := map[uint32][]sample{}
	bytes := map[uint32]int64{}
	cuts := map[uint32]int{}
	states := map[uint32][]stateChange{}

	ctx, cancel := context.WithTimeout(context.Background(), *duration+10*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, *addr, client.Options{
		Token: *token, ClientVersion: "netbench/dev", TLS: &tls.Config{InsecureSkipVerify: *insecure},
		OnFrame: func(f client.RemoteFrame) {
			now := time.Now()
			mu.Lock()
			samples[f.TrackID] = append(samples[f.TrackID], sample{at: now.Sub(start), layer: f.Layer,
				latency: now.Sub(time.UnixMicro(int64(f.CaptureUS)))})
			if now.Sub(start) >= measureFrom(*warmup, *eventAt) {
				bytes[f.TrackID] += int64(len(f.Payload))
			}
			mu.Unlock()
		},
		OnGroupEnd: func(id, _ uint32, _ uint64, complete bool) {
			if !complete {
				mu.Lock()
				cuts[id]++
				mu.Unlock()
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	st, err := c.Join(ctx, *room)
	if err != nil {
		log.Fatal(err)
	}
	tracks := map[uint32]*controlpb.Track{}
	sub := func(t *controlpb.Track) {
		tracks[t.GetTrackId()] = t
		_ = c.Subscribe(t.GetTrackId(), topLayer(t), 0)
	}
	for _, t := range st.GetTracks() {
		sub(t)
	}
	timer := time.After(*duration)
loop:
	for {
		select {
		case <-timer:
			break loop
		case m, ok := <-c.Events():
			if !ok {
				log.Fatalf("connection closed: %v", c.Err())
			}
			if t := m.GetTrackAdded().GetTrack(); t != nil {
				sub(t)
			}
			if s := m.GetSubscriptionState(); s != nil {
				mu.Lock()
				states[s.GetTrackId()] = append(states[s.GetTrackId()], stateChange{
					At: time.Since(start).Seconds(), Layer: s.GetLayer(), Target: s.GetTargetLayer(),
					Paused: s.GetPaused(), Estimate: s.GetEstimateBps() / 1000})
				mu.Unlock()
			}
		}
	}
	end := time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	var results []trackResult
	ids := make([]uint32, 0, len(tracks))
	for id := range tracks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	from := measureFrom(*warmup, *eventAt)
	for _, id := range ids {
		t := tracks[id]
		r := analyze(*scenario, t, samples[id], from, end, *eventAt)
		r.Kbps = float64(bytes[id]) * 8 / (end - from).Seconds() / 1000
		r.Cuts = cuts[id]
		r.States = states[id]
		results = append(results, r)
		rec := "–"
		if r.RecoveryS != nil {
			rec = fmt.Sprintf("%.1f s", *r.RecoveryS)
		} else if r.Unrecovered {
			rec = "not recovered"
		}
		fmt.Printf("| %s | %s | %s | %.1f | %.0f | %.0f | %.0f | %.1f s | %s |\n",
			r.Scenario, r.Track, r.Layer, r.FPS, r.Kbps, r.P50ms, r.P99ms, r.FreezeS, rec)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func measureFrom(warmup, event time.Duration) time.Duration {
	if event > 0 {
		return event
	}
	return warmup
}

func topLayer(t *controlpb.Track) uint32 {
	var best *controlpb.Layer
	for _, l := range t.GetLayers() {
		if best == nil || l.GetBitrate() > best.GetBitrate() {
			best = l
		}
	}
	return best.GetId()
}

func layerInfo(t *controlpb.Track, id uint32) *controlpb.Layer {
	for _, l := range t.GetLayers() {
		if l.GetId() == id {
			return l
		}
	}
	return nil
}

func interval(t *controlpb.Track, layer uint32) time.Duration {
	if l := layerInfo(t, layer); l != nil && l.GetFps() > 0 {
		return time.Second / time.Duration(l.GetFps())
	}
	return 100 * time.Millisecond
}

func label(t *controlpb.Track, layer uint32) string {
	l := layerInfo(t, layer)
	if l == nil {
		return fmt.Sprint(layer)
	}
	return fmt.Sprintf("L%d %dp", layer, l.GetHeight())
}

func analyze(scenario string, t *controlpb.Track, all []sample, from, end, event time.Duration) trackResult {
	kind := strings.ToLower(strings.TrimPrefix(t.GetKind().String(), "TRACK_KIND_"))
	r := trackResult{Scenario: scenario, Track: kind}
	var win []sample
	for _, s := range all {
		if s.at >= from {
			win = append(win, s)
		}
	}
	span := (end - from).Seconds()
	if len(win) == 0 {
		r.Layer = "none"
		r.FreezeS = span
		if event > 0 {
			r.Unrecovered = true
		}
		return r
	}
	count := map[uint32]int{}
	lo, hi := win[0].layer, win[0].layer
	for _, s := range win {
		count[s.layer]++
		lo, hi = min(lo, s.layer), max(hi, s.layer)
	}
	mode := win[0].layer
	for l, n := range count {
		if n > count[mode] || (n == count[mode] && l > mode) {
			mode = l
		}
	}
	r.Layer = label(t, mode)
	if lo != hi {
		r.Layer += fmt.Sprintf(" (L%d–L%d)", lo, hi)
	}
	r.FPS = float64(len(win)) / span
	lats := make([]time.Duration, len(win))
	for i, s := range win {
		lats[i] = s.latency
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	r.P50ms = ms(lats[len(lats)/2])
	r.P99ms = ms(lats[min(len(lats)-1, len(lats)*99/100)])

	// Freeze: gaps beyond the layer's threshold, including the start and end.
	prev := from
	var freeze time.Duration
	for i, s := range win {
		limit := max(250*time.Millisecond, 3*interval(t, s.layer))
		gap := s.at - prev
		if i == 0 && event == 0 {
			gap = 0 // steady state: don't charge the window start
		}
		if gap > limit {
			freeze += gap - interval(t, s.layer)
		}
		prev = s.at
	}
	if tail := end - prev; tail > 250*time.Millisecond {
		freeze += tail
	}
	r.FreezeS = freeze.Seconds()

	if event > 0 {
		rec := recovery(t, win, event, end)
		if rec < 0 {
			r.Unrecovered = true
		} else {
			v := rec.Seconds()
			r.RecoveryS = &v
		}
	}
	return r
}

// recovery: first time after the event that starts 2 s of steady video.
func recovery(t *controlpb.Track, win []sample, event, end time.Duration) time.Duration {
	const span = 2 * time.Second
	for start := event; start+span <= end; start += 100 * time.Millisecond {
		var in []sample
		for _, s := range win {
			if s.at >= start && s.at < start+span {
				in = append(in, s)
			}
		}
		if len(in) == 0 {
			continue
		}
		layer := in[len(in)-1].layer
		need := 0.7 * span.Seconds() / interval(t, layer).Seconds()
		if float64(len(in)) < need {
			continue
		}
		lats := make([]time.Duration, len(in))
		for i, s := range in {
			lats[i] = s.latency
		}
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		if lats[len(lats)*9/10] < 400*time.Millisecond {
			return start - event
		}
	}
	return -1
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
