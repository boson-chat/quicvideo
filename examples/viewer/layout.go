package main

import "math"

// Rect is a tile's frame in points, origin top-left.
type Rect struct{ X, Y, W, H float64 }

// Tile is one remote track's place in the window.
type Tile struct {
	TrackID uint32
	Rect    Rect
	Visible bool
	Pinned  bool
}

const (
	gap        = 6.0
	maxStrip   = 6 // small tiles shown next to a pinned one
	maxGrid    = 9 // tiles shown without a pin
	stripShare = 0.22
)

// Layout places tracks (in display order) in a w×h window. With a pinned
// track, it gets the big area and the others a strip along the bottom (at
// most maxStrip, the rest hidden); otherwise a near-square grid of up to
// maxGrid tiles. Hidden tiles get no rect and should be unsubscribed;
// windowVisible=false (minimized/occluded) hides everything.
func Layout(tracks []uint32, pinned uint32, w, h float64, windowVisible bool) []Tile {
	tiles := make([]Tile, len(tracks))
	for i, id := range tracks {
		tiles[i] = Tile{TrackID: id, Pinned: id == pinned && pinned != 0}
	}
	if len(tracks) == 0 || w <= 0 || h <= 0 || !windowVisible {
		return tiles
	}
	pin := -1
	for i, t := range tiles {
		if t.Pinned {
			pin = i
		}
	}
	if pin >= 0 && len(tracks) > 1 {
		stripH := math.Max(90, h*stripShare)
		tiles[pin].Rect = Rect{gap, gap, w - 2*gap, h - stripH - 3*gap}
		tiles[pin].Visible = true
		others := make([]int, 0, len(tiles)-1)
		for i := range tiles {
			if i != pin {
				others = append(others, i)
			}
		}
		n := min(len(others), maxStrip)
		tw := (w - gap*float64(n+1)) / float64(n)
		tw = math.Min(tw, stripH*16/9)
		for k, i := range others {
			if k >= n {
				break
			}
			tiles[i].Rect = Rect{gap + float64(k)*(tw+gap), h - stripH - gap, tw, stripH}
			tiles[i].Visible = true
		}
		return tiles
	}
	n := min(len(tiles), maxGrid)
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := int(math.Ceil(float64(n) / float64(cols)))
	tw := (w - gap*float64(cols+1)) / float64(cols)
	th := (h - gap*float64(rows+1)) / float64(rows)
	for i := range n {
		r, c := i/cols, i%cols
		tiles[i].Rect = Rect{gap + float64(c)*(tw+gap), gap + float64(r)*(th+gap), tw, th}
		tiles[i].Visible = true
	}
	return tiles
}
