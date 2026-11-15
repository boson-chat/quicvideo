// pub publishes one simulcast track: synthetic frames (default) or real
// H.264 looped from Annex-B files (-h264, one file per layer; see
// `make testdata`).
//
//	go run ./examples/pub -insecure -token alice -room call
//	go run ./examples/pub -insecure -token alice -room call -h264 testdata/l0.h264,testdata/l1.h264,testdata/l2.h264
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
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/internal/h264"
	"github.com/boson-chat/quicvideo/internal/synth"
)

func main() {
	addr := flag.String("addr", "localhost:7710", "server address")
	token := flag.String("token", "", "token (with AUTH_INSECURE: any user id)")
	room := flag.String("room", "call", "room")
	insecure := flag.Bool("insecure", false, "skip TLS verification (self-signed dev server)")
	kind := flag.String("kind", "camera", "camera | screen")
	layersF := flag.String("layers", "320x180@15:200k,640x360@30:600k,1280x720@30:1500k", "simulcast layers WxH@fps:bitrate, lowest first")
	gop := flag.Duration("gop", 2*time.Second, "keyframe interval (synthetic mode)")
	files := flag.String("h264", "", "comma-separated Annex-B files, one per layer (file mode)")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until Ctrl-C)")
	flag.Parse()

	layers, err := synth.ParseLayers(*layersF)
	if err != nil {
		log.Fatal(err)
	}
	var aus [][]h264.AccessUnit
	if *files != "" {
		paths := strings.Split(*files, ",")
		if len(paths) != len(layers) {
			log.Fatalf("-h264 has %d files for %d layers", len(paths), len(layers))
		}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				log.Fatal(err)
			}
			a, err := h264.Split(data)
			if err != nil {
				log.Fatalf("%s: %v", p, err)
			}
			aus = append(aus, a)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	c, err := client.Dial(ctx, *addr, client.Options{Token: *token, ClientVersion: "pub/dev", TLS: &tls.Config{InsecureSkipVerify: *insecure}})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	go func() {
		for m := range c.Events() {
			if e := m.GetError(); e != nil {
				log.Printf("server error: %s %s", e.GetCode(), e.GetMessage())
			}
		}
	}()
	if _, err := c.Join(ctx, *room); err != nil {
		log.Fatal(err)
	}
	k := controlpb.TrackKind_CAMERA
	if *kind == "screen" {
		k = controlpb.TrackKind_SCREEN
	}
	track, err := c.Publish(ctx, k, "h264", layers)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("publishing track %d (%s, %d layers) in %q", track.ID(), *kind, len(layers), *room)

	var mu sync.Mutex
	sources := make([]*synth.Source, len(layers))
	for i, l := range layers {
		sources[i] = synth.NewSource(l, int(gop.Seconds()*float64(l.GetFps())))
	}
	// Keyframe needed (server request, layer activated, or our send queue
	// dropped frames): synthetic layers make one now; file layers jump to
	// the next IDR.
	jump := make([]bool, len(layers))
	go func() {
		for l := range track.KeyframeRequests() {
			if int(l) >= len(sources) {
				continue
			}
			mu.Lock()
			if aus == nil {
				sources[l].ForceKeyframe()
			} else {
				jump[l] = true
			}
			mu.Unlock()
		}
	}()

	var wg sync.WaitGroup
	for i, l := range layers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(time.Second / time.Duration(l.GetFps()))
			defer tick.Stop()
			n := 0
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if !track.LayerActive(uint32(i)) {
					// Nobody watches this layer: don't "encode" it at all.
					continue
				}
				var payload []byte
				var key bool
				if aus != nil {
					mu.Lock()
					if jump[i] {
						jump[i] = false
						for !aus[i][n%len(aus[i])].Keyframe {
							n++
						}
					}
					mu.Unlock()
					au := aus[i][n%len(aus[i])]
					payload, key = au.Data, au.Keyframe
					n++
				} else {
					mu.Lock()
					size, k := sources[i].Next()
					mu.Unlock()
					payload, key = synth.Payload(size), k
				}
				if err := track.WriteFrame(uint32(i), key, 0, payload); err != nil {
					continue // inactive, waiting for a keyframe, or backpressure (see stats)
				}
			}
		}()
	}
	start := time.Now()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-c.Done():
			log.Fatalf("connection closed: %v", c.Err())
		case <-t.C:
			var parts []string
			for _, st := range track.Stats() {
				state := "off"
				if st.Active {
					state = "on"
				}
				parts = append(parts, fmt.Sprintf("L%d %s %.0f kbps (dropped %d, queue %v)", st.Layer, state,
					float64(st.BytesSent)*8/time.Since(start).Seconds()/1000, st.FramesDropped, st.QueueDelay.Round(time.Millisecond)))
			}
			sort.Strings(parts)
			log.Print(strings.Join(parts, "  "))
		}
	}
}
