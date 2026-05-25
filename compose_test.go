package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// solidImage returns a w×h image filled with c.
func solidImage(w, h int, c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestCropToFillDimensions(t *testing.T) {
	cases := []struct {
		name string
		w, h int
	}{
		{"portrait", 600, 900},
		{"landscape", 1200, 800},
		{"square", 500, 500},
		{"tiny", 3, 7},
		{"exact-half", halfWidth, halfHeight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := cropToFill(solidImage(tc.w, tc.h, color.White), halfWidth, halfHeight)
			b := out.Bounds()
			if b.Dx() != halfWidth || b.Dy() != halfHeight {
				t.Fatalf("got %dx%d, want %dx%d", b.Dx(), b.Dy(), halfWidth, halfHeight)
			}
		})
	}
}

func TestComposeDimensions(t *testing.T) {
	left := solidImage(600, 900, color.RGBA{R: 255, A: 255})
	right := solidImage(1000, 700, color.RGBA{B: 255, A: 255})

	out := compose(left, right)
	b := out.Bounds()
	if b.Dx() != canvasWidth || b.Dy() != canvasHeight {
		t.Fatalf("composite is %dx%d, want %dx%d", b.Dx(), b.Dy(), canvasWidth, canvasHeight)
	}

	// The left half should be (mostly) red and the right half blue, confirming
	// the two images landed on their respective sides.
	lr, _, _, _ := out.At(halfWidth/2, canvasHeight/2).RGBA()
	_, _, rb, _ := out.At(halfWidth+halfWidth/2, canvasHeight/2).RGBA()
	if lr < 0x8000 {
		t.Errorf("left half not predominantly red (R=%d)", lr>>8)
	}
	if rb < 0x8000 {
		t.Errorf("right half not predominantly blue (B=%d)", rb>>8)
	}
}

// gradient returns a w×h image with a colour gradient, so dithering has to make
// real choices (a solid fill would map to one palette entry with no diffusion).
func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: uint8((x + y) * 255 / (w + h)), A: 255})
		}
	}
	return img
}

func TestAdjustColorsSaturationAndBrightness(t *testing.T) {
	// A muted mid-grey-ish blue: saturation should spread the channels apart and
	// brightness should lift the overall level.
	src := solidImage(4, 4, color.RGBA{R: 80, G: 90, B: 140, A: 255})

	// No-op fast path: identical pixels back.
	if out := adjustColors(src, 1, 1); out != image.Image(src) {
		t.Fatal("saturation=1, brightness=1 should return src unchanged")
	}

	// Saturation only (brightness=1): channels move away from their shared luma,
	// preserving luma but increasing the spread.
	satOnly := adjustColors(src, 2.0, 1)
	r0, g0, b0, _ := src.At(0, 0).RGBA()
	r1, g1, b1, _ := satOnly.At(0, 0).RGBA()
	spread := func(r, g, b uint32) uint32 { return max(r, max(g, b)) - min(r, min(g, b)) }
	if spread(r1>>8, g1>>8, b1>>8) <= spread(r0>>8, g0>>8, b0>>8) {
		t.Fatalf("saturation>1 did not increase channel spread: before %d, after %d",
			spread(r0>>8, g0>>8, b0>>8), spread(r1>>8, g1>>8, b1>>8))
	}

	// Brightness only (saturation=1): every channel should rise (gamma lift), and
	// nothing should exceed 255.
	bright := adjustColors(src, 1, 1.5)
	br, bg, bb, _ := bright.At(0, 0).RGBA()
	if br>>8 <= r0>>8 || bg>>8 <= g0>>8 || bb>>8 <= b0>>8 {
		t.Fatalf("brightness>1 did not lift channels: %d,%d,%d -> %d,%d,%d",
			r0>>8, g0>>8, b0>>8, br>>8, bg>>8, bb>>8)
	}

	// White stays white under a brightness lift (no clipping artefacts the other way).
	white := adjustColors(solidImage(2, 2, color.White), 1, 2.0)
	wr, wg, wb, _ := white.At(0, 0).RGBA()
	if wr>>8 != 255 || wg>>8 != 255 || wb>>8 != 255 {
		t.Fatalf("brightness lift moved white off 255: %d,%d,%d", wr>>8, wg>>8, wb>>8)
	}
}

func TestDitherProducesPaletteIndices(t *testing.T) {
	for _, algo := range []ditherAlgo{ditherFloydSteinberg, ditherAtkinson} {
		p := ditherImage(gradient(canvasWidth, canvasHeight), algo)
		b := p.Bounds()
		if b.Dx() != canvasWidth || b.Dy() != canvasHeight {
			t.Fatalf("algo %d: dithered image is %dx%d, want %dx%d", algo, b.Dx(), b.Dy(), canvasWidth, canvasHeight)
		}
		// The palette has 7 entries, so every pixel must be index 0–6 (never the
		// unused PEN 7); error diffusion can't emit an index outside the palette.
		for i, idx := range p.Pix {
			if int(idx) >= len(inkyPalette) {
				t.Fatalf("algo %d: pixel %d has palette index %d, out of range [0,%d)", algo, i, idx, len(inkyPalette))
			}
		}
	}
}

// TestAtkinsonDiffersFromFloydSteinberg confirms the Atkinson path is actually a
// distinct algorithm (it must not silently fall back to Floyd–Steinberg).
func TestAtkinsonDiffersFromFloydSteinberg(t *testing.T) {
	src := gradient(canvasWidth, canvasHeight)
	fs := ditherImage(src, ditherFloydSteinberg)
	at := ditherImage(src, ditherAtkinson)
	if bytes.Equal(fs.Pix, at.Pix) {
		t.Fatal("Atkinson and Floyd–Steinberg produced identical output")
	}
}

func TestPackFramebufferLength(t *testing.T) {
	fb := packFramebuffer(ditherImage(gradient(canvasWidth, canvasHeight), ditherFloydSteinberg))
	if want := canvasWidth * canvasHeight / 2; len(fb) != want {
		t.Fatalf("framebuffer is %d bytes, want %d", len(fb), want)
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	p := ditherImage(gradient(canvasWidth, canvasHeight), ditherAtkinson)
	got := unpackFramebuffer(packFramebuffer(p))
	if !bytes.Equal(got.Pix, p.Pix) {
		t.Fatal("pack/unpack did not round-trip the palette indices losslessly")
	}
}

func TestEncodePNGRoundTrip(t *testing.T) {
	fb := packFramebuffer(ditherImage(gradient(canvasWidth, canvasHeight), ditherFloydSteinberg))
	data, err := encodePNG(unpackFramebuffer(fb))
	if err != nil {
		t.Fatalf("encodePNG: %v", err)
	}

	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Width != canvasWidth || cfg.Height != canvasHeight {
		t.Fatalf("decoded PNG is %dx%d, want %dx%d", cfg.Width, cfg.Height, canvasWidth, canvasHeight)
	}
	if _, ok := cfg.ColorModel.(color.Palette); !ok {
		t.Fatalf("decoded PNG is not paletted, got %T", cfg.ColorModel)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
}
