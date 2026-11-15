// sub joins a room, subscribes to every track (or -track) at -layer, and
// prints per-track stats. -dump writes the received H.264 of one track to a
// file you can play: ffplay -f h264 out.h264
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
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

type stats struct {
	frames, bytes      int64
	groups, incomplete int64
	lat                []time.Duration
	layer              uint32
}

func main() {
	addr := flag.String("addr", "localhost:7710", "server address")
	token := flag.String("token", "", "token (with AUTH_INSECURE: any user id)")
	room := flag.String("room", "call", "room")
	insecure := flag.Bool("insecure", false, "skip TLS verification")
	layer := flag.Uint("layer", 99, "max layer to subscribe to (default: the top layer); the server picks within it")
	only := flag.Uint("track", 0, "only this track id (0 = all)")
	dump := flag.String("dump", "", "write the received bitstream of the first subscribed track to this file")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until Ctrl-C)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	var out *os.File
	if *dump != "" {
		f, err := os.Create(*dump)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		out = f
	}

	var mu sync.Mutex
	st := map[uint32]*stats{}
	var dumpTrack uint32
	dumpStarted := false
	get := func(id uint32) *stats {
		s := st[id]
		if s == nil {
			s = &stats{}
			st[id] = s
		}
		return s
	}

	c, err := client.Dial(ctx, *addr, client.Options{
		Token: *token, ClientVersion: "sub/dev", TLS: &tls.Config{InsecureSkipVerify: *insecure},
		OnFrame: func(f client.RemoteFrame) {
			lat := time.Since(time.UnixMicro(int64(f.CaptureUS)))
			mu.Lock()
			defer mu.Unlock()
			s := get(f.TrackID)
			s.frames++
			s.bytes += int64(len(f.Payload))
			s.layer = f.Layer
			if len(s.lat) < 100000 {
				s.lat = append(s.lat, lat)
			}
			if out != nil && f.TrackID == dumpTrack {
				if f.GroupStart {
					dumpStarted = true
				}
				if dumpStarted {
					_, _ = out.Write(f.Payload)
				}
			}
		},
		OnGroupEnd: func(id, _ uint32, _ uint64, complete bool) {
			mu.Lock()
			defer mu.Unlock()
			s := get(id)
			s.groups++
			if !complete {
				s.incomplete++
				if id == dumpTrack {
					dumpStarted = false // resume at the next keyframe
				}
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	state, err := c.Join(ctx, *room)
	if err != nil {
		log.Fatal(err)
	}
	subscribe := func(t *controlpb.Track) {
		if *only != 0 && t.GetTrackId() != uint32(*only) {
			return
		}
		mu.Lock()
		if out != nil && dumpTrack == 0 {
			dumpTrack = t.GetTrackId()
		}
		mu.Unlock()
		l := uint32(*layer)
		if int(l) >= len(t.GetLayers()) {
			l = uint32(len(t.GetLayers()) - 1)
		}
		log.Printf("subscribing to track %d (%s from session %d) layer %d", t.GetTrackId(), t.GetKind(), t.GetSessionId(), l)
		if err := c.Subscribe(t.GetTrackId(), l, 0); err != nil {
			log.Print(err)
		}
	}
	for _, t := range state.GetTracks() {
		subscribe(t)
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-c.Events():
			if !ok {
				log.Fatalf("connection closed: %v", c.Err())
			}
			switch {
			case m.GetTrackAdded() != nil:
				subscribe(m.GetTrackAdded().GetTrack())
			case m.GetSubscriptionState() != nil:
				st := m.GetSubscriptionState()
				log.Printf("track %d: layer %d (target %d, max %d) forwarding=%v paused=%v estimate %d kbps",
					st.GetTrackId(), st.GetLayer(), st.GetTargetLayer(), st.GetMaxLayer(), st.GetForwarding(), st.GetPaused(), st.GetEstimateBps()/1000)
			case m.GetTrackRemoved() != nil:
				log.Printf("track %d removed", m.GetTrackRemoved().GetTrackId())
			case m.GetError() != nil:
				log.Printf("server error: %s %s", m.GetError().GetCode(), m.GetError().GetMessage())
			}
		case <-tick.C:
			dt := time.Since(last).Seconds()
			last = time.Now()
			mu.Lock()
			for id, s := range st {
				p50, p99 := percentiles(s.lat)
				fmt.Printf("track %d L%d: %.1f fps %.0f kbps latency p50 %v p99 %v groups %d (dropped %d)\n",
					id, s.layer, float64(s.frames)/dt, float64(s.bytes)*8/dt/1000, p50, p99, s.groups, s.incomplete)
				s.frames, s.bytes, s.lat = 0, 0, s.lat[:0]
			}
			mu.Unlock()
		}
	}
}

func percentiles(d []time.Duration) (p50, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2].Round(100 * time.Microsecond), s[len(s)*99/100].Round(100 * time.Microsecond)
}
