// loadtest simulates video calls against a running quicvideod: -rooms calls
// with -n participants each. Every participant publishes a synthetic
// simulcast camera and subscribes to everyone else's at -sub-layer; -viewers
// adds watch-only participants per room. It prints aggregate receive
// throughput, latency and drops; measure the server's CPU separately.
//
//	go run ./examples/loadtest -insecure -rooms 4 -n 6 -duration 30s
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/internal/synth"
)

type agg struct {
	rxBytes, rxFrames, groups, incomplete atomic.Int64
	mu                                    sync.Mutex
	lat                                   []time.Duration
	measuring                             atomic.Bool
}

func (a *agg) frame(f client.RemoteFrame) {
	if !a.measuring.Load() {
		return
	}
	a.rxBytes.Add(int64(len(f.Payload)))
	a.rxFrames.Add(1)
	lat := time.Since(time.UnixMicro(int64(f.CaptureUS)))
	a.mu.Lock()
	if len(a.lat) < 2_000_000 {
		a.lat = append(a.lat, lat)
	}
	a.mu.Unlock()
}

func (a *agg) groupEnd(complete bool) {
	if !a.measuring.Load() {
		return
	}
	a.groups.Add(1)
	if !complete {
		a.incomplete.Add(1)
	}
}

func main() {
	addr := flag.String("addr", "localhost:7710", "server address")
	insecure := flag.Bool("insecure", false, "skip TLS verification")
	rooms := flag.Int("rooms", 2, "concurrent calls")
	n := flag.Int("n", 4, "publishing participants per call")
	viewers := flag.Int("viewers", 0, "extra watch-only participants per call")
	layersF := flag.String("layers", "320x180@15:150k,640x360@30:500k,1280x720@30:1200k", "simulcast layers per camera")
	subLayer := flag.Int("sub-layer", 1, "layer subscribers take (-1 = top)")
	gop := flag.Duration("gop", 2*time.Second, "keyframe interval")
	warmup := flag.Duration("warmup", 3*time.Second, "excluded from the results")
	duration := flag.Duration("duration", 20*time.Second, "measured duration")
	flag.Parse()

	layers, err := synth.ParseLayers(*layersF)
	if err != nil {
		log.Fatal(err)
	}
	sl := *subLayer
	if sl < 0 || sl >= len(layers) {
		sl = len(layers) - 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *warmup+*duration+5*time.Second)
	defer cancel()

	a := &agg{}
	var wg sync.WaitGroup
	var txBytes atomic.Int64
	tlsConf := &tls.Config{InsecureSkipVerify: *insecure}

	for r := range *rooms {
		room := fmt.Sprintf("load-%d", r)
		for p := range *n + *viewers {
			publish := p < *n
			wg.Add(1)
			go func() {
				defer wg.Done()
				user := fmt.Sprintf("u%d-%d", r, p)
				if !publish {
					user += "@listener"
				}
				c, err := client.Dial(ctx, *addr, client.Options{
					Token: user, TLS: tlsConf, ClientVersion: "loadtest/dev", OnFrame: a.frame,
					OnGroupEnd: func(_, _ uint32, _ uint64, complete bool) { a.groupEnd(complete) },
				})
				if err != nil {
					log.Printf("%s: %v", user, err)
					return
				}
				defer c.Close()
				state, err := c.Join(ctx, room)
				if err != nil {
					log.Printf("%s join: %v", user, err)
					return
				}
				for _, t := range state.GetTracks() {
					_ = c.Subscribe(t.GetTrackId(), uint32(sl), 0)
				}
				go func() {
					for m := range c.Events() {
						if t := m.GetTrackAdded().GetTrack(); t != nil {
							_ = c.Subscribe(t.GetTrackId(), uint32(sl), 0)
						}
					}
				}()
				if publish {
					runPublisher(ctx, c, layers, *gop, &txBytes)
				} else {
					<-ctx.Done()
				}
			}()
			time.Sleep(10 * time.Millisecond) // stagger connects
		}
	}

	time.Sleep(*warmup)
	a.measuring.Store(true)
	tx0 := txBytes.Load()
	start := time.Now()
	time.Sleep(*duration)
	a.measuring.Store(false)
	el := time.Since(start).Seconds()
	tx := txBytes.Load() - tx0
	cancel()
	wg.Wait()

	a.mu.Lock()
	sort.Slice(a.lat, func(i, j int) bool { return a.lat[i] < a.lat[j] })
	p := func(q float64) time.Duration {
		if len(a.lat) == 0 {
			return 0
		}
		return a.lat[int(float64(len(a.lat)-1)*q)].Round(100 * time.Microsecond)
	}
	streams := *rooms * *n * (*n - 1 + *viewers)
	fmt.Printf("calls %d × (%d publishing + %d watching), layer %d (%s), %d forwarded streams\n",
		*rooms, *n, *viewers, sl, fmtRate(float64(layers[sl].GetBitrate())), streams)
	fmt.Printf("publish (all layers): %s   receive: %s   expected receive ≈ %s\n",
		fmtRate(float64(tx)*8/el), fmtRate(float64(a.rxBytes.Load())*8/el), fmtRate(float64(streams)*float64(layers[sl].GetBitrate())))
	fmt.Printf("frames %d (%.0f/s)  latency p50 %v p90 %v p99 %v max %v\n",
		a.rxFrames.Load(), float64(a.rxFrames.Load())/el, p(0.5), p(0.9), p(0.99), p(1))
	fmt.Printf("groups %d, cut short %d\n", a.groups.Load(), a.incomplete.Load())
	a.mu.Unlock()
}

func runPublisher(ctx context.Context, c *client.Client, layers []*controlpb.Layer, gop time.Duration, tx *atomic.Int64) {
	track, err := c.Publish(ctx, controlpb.TrackKind_CAMERA, "h264", layers)
	if err != nil {
		log.Printf("publish: %v", err)
		return
	}
	var mu sync.Mutex
	srcs := make([]*synth.Source, len(layers))
	for i, l := range layers {
		srcs[i] = synth.NewSource(l, int(gop.Seconds()*float64(l.GetFps())))
	}
	go func() {
		for l := range track.KeyframeRequests() {
			if int(l) < len(srcs) {
				mu.Lock()
				srcs[l].ForceKeyframe()
				mu.Unlock()
			}
		}
	}()
	var wg sync.WaitGroup
	for i, l := range layers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(time.Second / time.Duration(l.GetFps()))
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				mu.Lock()
				size, key := srcs[i].Next()
				mu.Unlock()
				if err := track.WriteFrame(uint32(i), key, 0, synth.Payload(size)); err == nil {
					tx.Add(int64(size))
				}
			}
		}()
	}
	wg.Wait()
}

func fmtRate(bps float64) string {
	if bps >= 1e6 {
		return fmt.Sprintf("%.1f Mbps", bps/1e6)
	}
	return fmt.Sprintf("%.0f kbps", bps/1e3)
}
