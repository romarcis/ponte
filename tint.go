package main

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
)

// tintPNG repaints the icon (violet mixed with white) in color (RGB),
// keeping the white parts and the transparency.
func tintPNG(b []byte, color uint32) []byte {
	src, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return b
	}
	img := image.NewNRGBA(src.Bounds())
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
	tr, tg, tb := float64(color>>16&0xff), float64(color>>8&0xff), float64(color&0xff)
	for i := 0; i+3 < len(img.Pix); i += 4 {
		// Green is 91 in both violets of the icon and 255 in white, so it
		// tells how much white each pixel has.
		k := min(max((float64(img.Pix[i+1])-91)/(255-91), 0), 1)
		img.Pix[i] = uint8(tr + (255-tr)*k)
		img.Pix[i+1] = uint8(tg + (255-tg)*k)
		img.Pix[i+2] = uint8(tb + (255-tb)*k)
	}
	var out bytes.Buffer
	if png.Encode(&out, img) != nil {
		return b
	}
	return out.Bytes()
}
