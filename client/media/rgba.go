package media

import "image"

// bgraToRGBA converts (and box-downscales) a top-down BGRA buffer to RGBA.
func bgraToRGBA(src []byte, stride, sw, sh, dw, dh int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		sy := y * sh / dh
		row := src[sy*stride:]
		dst := img.Pix[y*img.Stride:]
		for x := range dw {
			sx := x * sw / dw * 4
			o := x * 4
			dst[o], dst[o+1], dst[o+2], dst[o+3] = row[sx+2], row[sx+1], row[sx], 255
		}
	}
	return img
}
