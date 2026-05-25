package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	"github.com/makeworld-the-better-one/dither/v2"
	"golang.org/x/image/draw"
)

// Output canvas dimensions and the per-image half.
const (
	canvasWidth  = 800
	canvasHeight = 480
	halfWidth    = canvasWidth / 2 // 400
	halfHeight   = canvasHeight    // 480
)

// inkyPalette is the Inky Frame 7.3"'s 7-colour ACeP palette. The slice INDEX is
// the panel's PEN number and is the firmware contract — do NOT reorder. (PicoGraphics
// pen 7, "clean"/taupe, is intentionally omitted so dithering never emits it.)
//
// The RGB values approximate what the panel *physically* displays, not pure
// primaries: because we dither on the server and ship raw pen indices, the
// dither target should match real panel output so error diffusion makes good
// choices (pure primaries make it oversaturate, since the panel can't show them).
// These are Pimoroni's measured values for the panel (the "saturated" set, kept
// alongside the primaries in pico_graphics.hpp's PicoGraphics_PenInky7). Tunable
// for quality without affecting the contract; only the index order is fixed.
var inkyPalette = color.Palette{
	color.RGBA{0x00, 0x00, 0x00, 0xFF}, // 0 black
	color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}, // 1 white
	color.RGBA{0x00, 0xFF, 0x00, 0xFF}, // 2 green
	color.RGBA{0x00, 0x00, 0xFF, 0xFF}, // 3 blue
	color.RGBA{0xFF, 0x00, 0x00, 0xFF}, // 4 red
	color.RGBA{0xFF, 0xFF, 0x00, 0xFF}, // 5 yellow
	color.RGBA{0xFF, 0x80, 0x00, 0xFF}, // 6 orange

	// Pimoroni's measured "saturated" panel values — swap in by uncommenting
	// (and commenting out the pure primaries above) if you prefer them.
	// color.RGBA{0x2b, 0x2a, 0x37, 0xFF}, // 0 black
	// color.RGBA{0xdc, 0xcb, 0xba, 0xFF}, // 1 white
	// color.RGBA{0x35, 0x56, 0x33, 0xFF}, // 2 green
	// color.RGBA{0x33, 0x31, 0x47, 0xFF}, // 3 blue
	// color.RGBA{0x9c, 0x3b, 0x2e, 0xFF}, // 4 red
	// color.RGBA{0xd3, 0xa9, 0x34, 0xFF}, // 5 yellow
	// color.RGBA{0xab, 0x58, 0x37, 0xFF}, // 6 orange
}

// cropToFill scales src to cover a w×h box and center-crops the overflow,
// preserving the source aspect ratio (no distortion, no padding). It does this
// in a single high-quality resampling step by scaling a centered source
// sub-rectangle (matching the w:h aspect ratio) onto the full destination.
func cropToFill(src image.Image, w, h int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()

	// Pick the largest centered sub-rectangle of src with aspect ratio w:h.
	// Compare sw/sh vs w/h using cross-multiplication to stay in integers.
	cropW, cropH := sw, sh
	if sw*h > sh*w {
		// Source is wider than target: limit width.
		cropW = sh * w / h
	} else {
		// Source is taller than (or equal to) target: limit height.
		cropH = sw * h / w
	}
	offX := sb.Min.X + (sw-cropW)/2
	offY := sb.Min.Y + (sh-cropH)/2
	srcRect := image.Rect(offX, offY, offX+cropW, offY+cropH)

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, srcRect, draw.Over, nil)
	return dst
}

// compose crops left and right to 400×480 each and places them side by side on
// an 800×480 canvas.
func compose(left, right image.Image) image.Image {
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))

	l := cropToFill(left, halfWidth, halfHeight)
	r := cropToFill(right, halfWidth, halfHeight)

	draw.Draw(canvas, image.Rect(0, 0, halfWidth, canvasHeight), l, image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(halfWidth, 0, canvasWidth, canvasHeight), r, image.Point{}, draw.Src)
	return canvas
}

// adjustColors boosts saturation and brightness of src before dithering, to
// counteract the Inky panel's dark, muted ACeP palette (otherwise photos render
// flat and dim). It runs in plain sRGB space — these are perceptual "slider"
// adjustments, not colour-managed ones:
//
//   - saturation scales each pixel's chroma around its Rec. 601 luma. 1.0 leaves
//     it unchanged, >1 pushes colours toward the vivid palette entries (so muted
//     regions stop collapsing to grey/black), 0 yields greyscale.
//   - brightness is a gamma lift: out = (in/255) ** (1/brightness). 1.0 is
//     unchanged, >1 lifts shadows and midtones while pinning white at white (no
//     highlight clipping). Applied after saturation so boosted colours brighten too.
//
// Returns src unchanged when both are 1.0 (a no-op fast path).
func adjustColors(src image.Image, saturation, brightness float64) image.Image {
	if saturation == 1 && brightness == 1 {
		return src
	}

	// Precompute the brightness gamma curve as a 256-entry lookup table.
	var gamma [256]uint8
	exp := 1.0 / brightness
	for i := range gamma {
		gamma[i] = clamp8(math.Pow(float64(i)/255, exp) * 255)
	}

	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := src.At(x, y).RGBA() // 16-bit, alpha-premultiplied
			rf, gf, bf := float64(r>>8), float64(g>>8), float64(bl>>8)

			luma := 0.299*rf + 0.587*gf + 0.114*bf
			rf = luma + (rf-luma)*saturation
			gf = luma + (gf-luma)*saturation
			bf = luma + (bf-luma)*saturation

			dst.SetRGBA(x, y, color.RGBA{
				R: gamma[clamp8(rf)],
				G: gamma[clamp8(gf)],
				B: gamma[clamp8(bf)],
				A: uint8(a >> 8),
			})
		}
	}
	return dst
}

// clamp8 rounds v to the nearest integer and clamps it to a uint8 [0,255].
func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}

// ditherAlgo selects the error-diffusion algorithm used to quantise to the
// 7-colour palette. Configured via the top-level "dither" config key.
type ditherAlgo int

const (
	ditherFloydSteinberg ditherAlgo = iota // default; smooth gradients
	ditherAtkinson                         // higher contrast, cleaner on e-ink
)

// dither maps src onto the 7-colour inkyPalette via error diffusion, producing
// an 800×480 paletted image whose pixel values are PEN indices 0–6. It uses the
// dither library, which works in linear (gamma-correct) RGB with perceptually
// weighted colour matching — important for a sparse palette. Serpentine scanning
// reduces directional artefacts.
func ditherImage(src image.Image, algo ditherAlgo) *image.Paletted {
	d := dither.NewDitherer(inkyPalette)
	d.Serpentine = true
	switch algo {
	case ditherAtkinson:
		d.Matrix = dither.Atkinson
	default:
		d.Matrix = dither.FloydSteinberg
	}
	// DitherPaletted returns an *image.Paletted whose indices align with
	// inkyPalette's order (= PEN numbers), so packFramebuffer can use them directly.
	return d.DitherPaletted(src)
}

// packFramebuffer packs a dithered image into the panel's 4-bits-per-pixel
// framebuffer: row-major, two pixels per byte, the first (left) pixel of each
// pair in the high nibble. For 800×480 that is exactly 192,000 bytes. This
// nibble order is part of the firmware contract. Relies on an even canvasWidth
// so each row is a whole number of bytes (800 → 400 bytes/row, no padding).
func packFramebuffer(p *image.Paletted) []byte {
	fb := make([]byte, canvasWidth*canvasHeight/2)
	for i := 0; i < len(fb); i++ {
		hi := p.Pix[2*i] & 0x0F
		lo := p.Pix[2*i+1] & 0x0F
		fb[i] = hi<<4 | lo
	}
	return fb
}

// unpackFramebuffer is the inverse of packFramebuffer: it reconstructs the exact
// paletted image from a 4bpp framebuffer (used to render a browser-friendly PNG).
func unpackFramebuffer(fb []byte) *image.Paletted {
	p := image.NewPaletted(image.Rect(0, 0, canvasWidth, canvasHeight), inkyPalette)
	for i, b := range fb {
		p.Pix[2*i] = b >> 4
		p.Pix[2*i+1] = b & 0x0F
	}
	return p
}

// encodePNG encodes a paletted image as an indexed PNG. With 7 palette entries
// this is a 4-bit colour-mapped PNG: tiny, lossless, and viewable in a browser.
func encodePNG(p *image.Paletted) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
