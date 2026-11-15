// captest checks capture + encode on this machine: it starts the camera or
// a screen share, encodes what arrives with the platform's default encoder
// at one layer, and reports frames, encoded frames and the encoder used.
// -png saves one captured frame.
//
//	go run ./examples/captest -source screen -duration 10s -png shot.png
package main

import (
	"flag"
	"image/png"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvideo/client/media"
)

func main() {
	source := flag.String("source", "screen", "screen | camera | pattern")
	cameraID := flag.String("camera-id", "", "camera id")
	duration := flag.Duration("duration", 10*time.Second, "how long")
	pngPath := flag.String("png", "", "save one captured frame here")
	flag.Parse()

	var captured, encoded, keys atomic.Int64
	var bytes atomic.Int64
	var once sync.Once
	var mu sync.Mutex
	var enc media.Encoder
	var size [2]int
	push := func(f media.CapturedFrame) {
		captured.Add(1)
		once.Do(func() {
			w, h := f.Picture.Size()
			size = [2]int{w, h}
			lw, lh := w, h
			if lw > 1280 {
				lw, lh = 1280, h*1280/w&^1
			}
			e, err := media.DefaultEncoder(media.EncoderConfig{Width: lw, Height: lh, FPS: 30, Bitrate: 1_500_000, GOP: 2 * time.Second},
				func(ef media.EncodedFrame) {
					encoded.Add(1)
					bytes.Add(int64(len(ef.Data)))
					if ef.Keyframe {
						keys.Add(1)
					}
				})
			if err != nil {
				log.Printf("encoder: %v", err)
				return
			}
			mu.Lock()
			enc = e
			mu.Unlock()
			if *pngPath != "" {
				if img, err := media.ToRGBA(f.Picture, 960); err == nil {
					if out, err := os.Create(*pngPath); err == nil {
						_ = png.Encode(out, img)
						out.Close()
					}
				}
			}
		})
		mu.Lock()
		e := enc
		mu.Unlock()
		if e != nil {
			_ = e.Encode(f.Picture, f.CaptureUS, false)
		}
	}
	var src media.Source
	var err error
	start := time.Now()
	switch *source {
	case "camera":
		src, err = media.StartCamera(media.CameraOptions{ID: *cameraID}, push)
	case "pattern":
		src, err = media.NewPatternSource(1280, 720, 30, push)
	default:
		src, err = media.StartScreen(media.ScreenOptions{DisplayID: media.PortalPicker, FPS: 30, Cursor: true}, push)
	}
	if err != nil {
		log.Fatalf("start %s: %v", *source, err)
	}
	log.Printf("%s started after %v", *source, time.Since(start).Round(time.Millisecond))
	time.Sleep(*duration)
	src.Stop()
	mu.Lock()
	if enc != nil {
		enc.Close()
	}
	mu.Unlock()
	backend := "?"
	if b, ok := enc.(interface{ Backend() string }); ok {
		backend = b.Backend()
	}
	log.Printf("%s %dx%d: %d frames captured (%.1f fps), %d encoded by %s (%d keyframes, %.0f kbps)",
		*source, size[0], size[1], captured.Load(), float64(captured.Load())/duration.Seconds(), encoded.Load(), backend,
		keys.Load(), float64(bytes.Load())*8/duration.Seconds()/1000)
}
