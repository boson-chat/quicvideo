package main

import (
	"image"
	"image/color"
)

var (
	colorBG   = color.RGBA{15, 17, 21, 255}
	colorTile = color.RGBA{31, 31, 31, 255}
)

// drawFit draws src into dst's rect r, scaled to fit (aspect kept,
// centered, nearest neighbour; good enough for a snapshot).
func drawFit(dst *image.RGBA, r image.Rectangle, src *image.RGBA) {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw == 0 || sh == 0 || r.Empty() {
		return
	}
	s := min(float64(r.Dx())/float64(sw), float64(r.Dy())/float64(sh))
	w, h := int(float64(sw)*s), int(float64(sh)*s)
	ox, oy := r.Min.X+(r.Dx()-w)/2, r.Min.Y+(r.Dy()-h)/2
	for y := range h {
		sy := min(int(float64(y)/s), sh-1)
		for x := range w {
			sx := min(int(float64(x)/s), sw-1)
			dst.SetRGBA(ox+x, oy+y, src.RGBAAt(sx, sy))
		}
	}
}
