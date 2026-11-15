package main

import (
	"image"
	"testing"

	"github.com/boson-chat/quicvideo/client/media"
)

func TestLayoutGrid(t *testing.T) {
	tiles := Layout([]uint32{1, 2, 3, 4}, 0, 1000, 600, true)
	for _, tl := range tiles {
		if !tl.Visible || tl.Rect.W < 480 || tl.Rect.H < 280 {
			t.Fatalf("2×2 grid: %+v", tl)
		}
	}
	many := make([]uint32, 12)
	for i := range many {
		many[i] = uint32(i + 1)
	}
	vis := 0
	for _, tl := range Layout(many, 0, 1200, 800, true) {
		if tl.Visible {
			vis++
		}
	}
	if vis != maxGrid {
		t.Fatalf("12 tracks: %d visible, want %d", vis, maxGrid)
	}
}

func TestLayoutPinned(t *testing.T) {
	tiles := Layout([]uint32{1, 2, 3}, 2, 1280, 800, true)
	big, small := tiles[1], tiles[0]
	if !big.Pinned || big.Rect.W < 1200 || big.Rect.H < 550 {
		t.Fatalf("pinned: %+v", big)
	}
	if !small.Visible || small.Rect.H > 200 || small.Rect.Y < big.Rect.Y+big.Rect.H {
		t.Fatalf("strip: %+v", small)
	}
	// Window hidden (minimized/occluded): nothing visible → all paused.
	for _, tl := range Layout([]uint32{1, 2, 3}, 2, 1280, 800, false) {
		if tl.Visible {
			t.Fatal("hidden window must hide every tile")
		}
	}
}

// Tile size → layer: the pinned tile asks for the top layer; strip tiles
// (313×176 points) for the lowest at 1x and the middle one at Retina 2x.
func TestTilesToLayers(t *testing.T) {
	layers := media.CameraLayers(1280, 720, 30)
	tiles := Layout([]uint32{1, 2, 3}, 1, 1280, 800, true)
	for scale, want := range map[float64]map[uint32]uint32{1: {1: 2, 2: 0, 3: 0}, 2: {1: 2, 2: 1, 3: 1}} {
		for _, tl := range tiles {
			l, ok := media.LayerForTile(layers, int(tl.Rect.W*scale), int(tl.Rect.H*scale), tl.Visible)
			if !ok || l != want[tl.TrackID] {
				t.Errorf("scale %v track %d tile %.0fx%.0f: layer %d ok=%v, want %d", scale, tl.TrackID, tl.Rect.W, tl.Rect.H, l, ok, want[tl.TrackID])
			}
		}
	}
	// A 2×2 grid at 1x on a small window: mid layer.
	tiles = Layout([]uint32{1, 2, 3, 4}, 0, 1000, 600, true)
	if l, _ := media.LayerForTile(layers, int(tiles[0].Rect.W), int(tiles[0].Rect.H), true); l != 1 {
		t.Errorf("grid tile at 1x: layer %d, want 1", l)
	}
}

func TestDrawFit(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for i := range src.Pix {
		src.Pix[i] = 200
	}
	dst := image.NewRGBA(image.Rect(0, 0, 400, 400))
	drawFit(dst, image.Rect(0, 0, 400, 400), src) // letterboxed: 400×225 centered
	if dst.RGBAAt(200, 200).R != 200 || dst.RGBAAt(200, 50).R != 0 || dst.RGBAAt(200, 300).R != 200 {
		t.Fatalf("letterbox wrong: %v %v %v", dst.RGBAAt(200, 200), dst.RGBAAt(200, 50), dst.RGBAAt(200, 300))
	}
}
