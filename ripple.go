package main

import "math"

// Circles drawn around the pointer when it moves from one computer to the
// other: on arrival on the controlled computer and when it comes back.

const (
	rippleRings        = 3
	defaultRippleColor = 0x5b5bf7 // the window's default accent color, RGB
)

// rippleFrame fills buf (size×size, premultiplied ARGB) with the circles at
// time t of the animation, from 0 (start) to 1 (gone), in color (RGB).
func rippleFrame(buf []uint32, size int, t, width float64, color uint32) {
	c := float64(size-1) / 2
	maxR := c - width
	cr, cg, cb := float64(color>>16&0xff), float64(color>>8&0xff), float64(color&0xff)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)-c, float64(y)-c)
			a := 0.0
			for i := 0; i < rippleRings; i++ {
				// Each ring starts a little after the previous one and
				// fades as it grows.
				p := t*1.6 - float64(i)*0.3
				if p <= 0 || p >= 1 {
					continue
				}
				cover := math.Min(math.Max(width/2-math.Abs(d-p*maxR)+0.5, 0), 1)
				a = math.Max(a, cover*(1-p))
			}
			buf[y*size+x] = uint32(a*255)<<24 | uint32(a*cr)<<16 | uint32(a*cg)<<8 | uint32(a*cb)
		}
	}
}

// rippleFx shows the circles; tests replace it.
var rippleFx = showRipple
