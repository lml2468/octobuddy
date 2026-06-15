package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// octopusTemplatePNG draws the XClaw octopus as a monochrome template image for
// the macOS menu bar: alpha encodes the silhouette (macOS tints it white/dark
// to match the bar), eyes punched out. Rendered at 2x (44px) for retina.
func octopusTemplatePNG() []byte {
	const s = 44
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	black := color.RGBA{0, 0, 0, 255}

	fillEllipse := func(cx, cy, rx, ry float64, c color.RGBA) {
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				dx := (float64(x) + 0.5 - cx) / rx
				dy := (float64(y) + 0.5 - cy) / ry
				if dx*dx+dy*dy <= 1 {
					img.Set(x, y, c)
				}
			}
		}
	}

	// Mantle / head — compact, sitting in the upper half.
	fillEllipse(22, 17, 13, 12, black)
	// Tentacle skirt: five narrow legs hanging low with small gaps, so the
	// silhouette reads as an octopus rather than a rounded blob at bar size.
	fillEllipse(10, 28, 2.8, 12, black)
	fillEllipse(16, 31, 2.8, 13, black)
	fillEllipse(22, 32, 2.8, 13, black)
	fillEllipse(28, 31, 2.8, 13, black)
	fillEllipse(34, 28, 2.8, 12, black)
	// Eyes punched out (alpha 0) — small, so they don't dominate.
	clear := color.RGBA{0, 0, 0, 0}
	fillEllipse(17.5, 16, 1.9, 2.4, clear)
	fillEllipse(26.5, 16, 1.9, 2.4, clear)

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
