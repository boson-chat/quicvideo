package media

var bars = [8][3]byte{{235, 235, 235}, {235, 235, 16}, {16, 235, 235}, {16, 235, 16}, {235, 16, 235}, {235, 16, 16}, {16, 16, 235}, {16, 16, 16}}

// drawPatternBGRA draws test pattern frame n into a BGRA buffer: colour
// bars, a box sweeping along the bottom quarter, and the frame counter as
// 16 bits across the top strip.
func drawPatternBGRA(pix []byte, stride, w, h, n int) {
	box := (n * 8) % max(w-h/4, 1)
	for y := range h {
		row := pix[y*stride:]
		for x := range w {
			c := bars[x*8/w]
			if y > h*3/4 {
				c = [3]byte{40, 40, 40}
				if x >= box && x < box+h/4 {
					c = [3]byte{255, 160, 0}
				}
			}
			if y < h/20 {
				if n>>(15-x*16/w)&1 == 1 {
					c = [3]byte{255, 255, 255}
				} else {
					c = [3]byte{0, 0, 0}
				}
			}
			o := x * 4
			row[o], row[o+1], row[o+2], row[o+3] = c[2], c[1], c[0], 255
		}
	}
}
