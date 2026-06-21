package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// xMarkTemplatePNG draws a stylized, designed "X" letterform as a monochrome
// template image for the macOS menu bar (macOS tints it white/dark to match the
// bar). It reads as an X — two crossing bars with open triangular notches — but
// with design tension: an italic shear plus unequal stroke weights (a heavier
// "\" and a lighter "/"). Rendered at 2x (44px), 3x-supersampled for clean
// diagonals.
func xMarkTemplatePNG() []byte {
	const (
		s      = 44
		center = 22.0
		shear  = 0.16 // italic lean
		L, R   = 5.0, 39.0
		T, B   = 6.0, 38.0
		wBack  = 14.0 // "\" stroke — heavier
		wFwd   = 8.5  // "/" stroke — lighter
	)

	shearQ := func(q [4][2]float64) [4][2]float64 {
		for i := range q {
			q[i][0] -= shear * (q[i][1] - center)
		}
		return q
	}
	// Constant-width bars with flat top/bottom caps.
	back := shearQ([4][2]float64{{L, T}, {L + wBack, T}, {R, B}, {R - wBack, B}}) // top-left → bottom-right
	fwd := shearQ([4][2]float64{{R - wFwd, T}, {R, T}, {L + wFwd, B}, {L, B}})    // top-right → bottom-left

	inside := func(q [4][2]float64, px, py float64) bool {
		sign := 0
		for i := range 4 {
			x1, y1 := q[i][0], q[i][1]
			x2, y2 := q[(i+1)%4][0], q[(i+1)%4][1]
			cross := (x2-x1)*(py-y1) - (y2-y1)*(px-x1)
			cs := 0
			switch {
			case cross > 0:
				cs = 1
			case cross < 0:
				cs = -1
			}
			if cs != 0 {
				if sign == 0 {
					sign = cs
				} else if sign != cs {
					return false
				}
			}
		}
		return true
	}

	img := image.NewRGBA(image.Rect(0, 0, s, s))
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			hits := 0
			for sy := 0; sy < 3; sy++ {
				for sx := 0; sx < 3; sx++ {
					px := float64(x) + (float64(sx)+0.5)/3
					py := float64(y) + (float64(sy)+0.5)/3
					if inside(back, px, py) || inside(fwd, px, py) {
						hits++
					}
				}
			}
			if hits > 0 {
				img.Set(x, y, color.RGBA{0, 0, 0, uint8(255 * hits / 9)})
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
