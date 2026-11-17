package media

import (
	"math"
	"sort"

	"github.com/boson-chat/quicvideo/gen/controlpb"
)

// Bits per pixel per frame used to size layer bitrates for H.264 at
// low-latency settings: cameras need more than mostly-static screens.
const (
	cameraBPP = 0.042
	// Per pixel per frame. Screens move in bursts (a scroll, a video) and
	// sit still between them, when nothing is sent at all.
	screenBPP = 0.05
)

// CameraLayers is the simulcast ladder for a camera capturing w×h at fps:
// quarter size at half rate, half size, and full size (capped at 720p
// height for the top layer by default: friends calls rarely need more).
func CameraLayers(w, h, fps int) []*controlpb.Layer {
	if w <= 0 || h <= 0 || fps <= 0 {
		return nil
	}
	tw, th := fit(w, h, 1280, 720)
	return ladder([][3]int{
		{even(tw / 4), even(th / 4), max(fps/2, 1)},
		{even(tw / 2), even(th / 2), fps},
		{tw, th, fps},
	}, cameraBPP, 100_000, 2_500_000)
}

// ScreenLayersFor is the ladder for a screen share's mode: ScreenMotion is
// ScreenLayers (15 and 30 fps); ScreenDetail keeps 15 fps but goes up to
// 1440p at the top, for small text.
func ScreenLayersFor(w, h int, mode ScreenMode) []*controlpb.Layer {
	if mode != ScreenDetail {
		return ScreenLayers(w, h)
	}
	if w <= 0 || h <= 0 {
		return nil
	}
	lw, lh := fit(w, h, 1280, 1280)
	hw, hh := fit(w, h, 2560, 1440)
	if lw == hw {
		return ladder([][3]int{{lw, lh, 15}}, screenBPP, 400_000, 4_000_000)
	}
	return ladder([][3]int{{lw, lh, 15}, {hw, hh, 15}}, screenBPP, 400_000, 4_000_000)
}

// ScreenFPS is a screen share's capture rate and top layer's frame rate.
const ScreenFPS = 30

// ScreenLayers is the ladder for screen sharing w×h: a ≤1280-wide layer at
// 5 fps for small tiles and weak links, and a ≤1920-wide layer at 15 fps.
// Screens are mostly static text, so the bitrate is per pixel but the fps
// is low.
func ScreenLayers(w, h int) []*controlpb.Layer {
	if w <= 0 || h <= 0 {
		return nil
	}
	lw, lh := fit(w, h, 1280, 1280)
	hw, hh := fit(w, h, 1920, 1920)
	// Both layers move smoothly: a scroll or a video at 5 fps (the old
	// small layer, which phones get) looked like a slide show. 15 fps is
	// the floor for motion; the top layer gets the capture's 30.
	if lw == hw {
		return ladder([][3]int{{lw, lh, ScreenFPS}}, screenBPP, 400_000, 4_000_000)
	}
	return ladder([][3]int{{lw, lh, 15}, {hw, hh, ScreenFPS}}, screenBPP, 400_000, 4_000_000)
}

func ladder(steps [][3]int, bpp float64, floor, ceil uint64) []*controlpb.Layer {
	out := make([]*controlpb.Layer, 0, len(steps))
	for i, s := range steps {
		br := uint64(math.Round(float64(s[0]*s[1]*s[2])*bpp/10_000) * 10_000)
		br = min(max(br, floor), ceil)
		if i > 0 && br <= out[i-1].GetBitrate() {
			br = out[i-1].GetBitrate() + 50_000
		}
		out = append(out, &controlpb.Layer{Id: uint32(i), Width: uint32(s[0]), Height: uint32(s[1]), Fps: uint32(s[2]), Bitrate: br})
	}
	return out
}

// fit scales w×h down (never up) to fit maxW×maxH, keeping the aspect
// ratio, with even dimensions (H.264 4:2:0).
func fit(w, h, maxW, maxH int) (int, int) {
	s := min(1, float64(maxW)/float64(w), float64(maxH)/float64(h))
	return even(int(math.Round(float64(w) * s))), even(int(math.Round(float64(h) * s)))
}

func even(v int) int { return max(v&^1, 2) }

// LayerForTile picks the max layer to subscribe to for a tile showing a
// track at tileW×tileH device pixels: the smallest layer that, fitted into
// the tile (aspect kept), is upscaled by at most ~11%; or the top layer if
// none is big enough. A tile that isn't visible should be unsubscribed
// instead (ok=false).
func LayerForTile(layers []*controlpb.Layer, tileW, tileH int, visible bool) (layer uint32, ok bool) {
	if !visible || tileW <= 0 || tileH <= 0 || len(layers) == 0 {
		return 0, false
	}
	sorted := append([]*controlpb.Layer(nil), layers...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].GetWidth()*sorted[i].GetHeight() < sorted[j].GetWidth()*sorted[j].GetHeight()
	})
	for _, l := range sorted {
		lw, lh := float64(l.GetWidth()), float64(l.GetHeight())
		if lw <= 0 || lh <= 0 {
			continue
		}
		if scale := min(float64(tileW)/lw, float64(tileH)/lh); scale <= 1/0.9 {
			return l.GetId(), true
		}
	}
	return sorted[len(sorted)-1].GetId(), true
}
