package image_test

import (
	"bytes"
	"image/color"
	"testing"

	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/media"
)

// banded is a 400×400 PNG: a red band on top, a blue one below, and a
// black-and-white checkerboard of 16 px squares between.
func banded(t *testing.T) []byte {
	return encodePNG(t, paint(400, 400, func(x, y int) color.RGBA {
		switch {
		case y < 100:
			return red
		case y >= 300:
			return blue
		case (x/16+y/16)%2 == 0:
			return color.RGBA{0, 0, 0, 255}
		}
		return white
	}))
}

// detail is a WebP's mean absolute difference between horizontal
// neighbours' luma: high for sharp edges, near zero once blurred.
func detail(t *testing.T, b []byte) float64 {
	t.Helper()
	img, err := webp.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	luma := func(x, y int) float64 {
		r, g, bl, _ := img.At(x, y).RGBA()
		return (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
	}
	r := img.Bounds()
	sum, n := 0.0, 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X + 1; x < r.Max.X; x++ {
			d := luma(x, y) - luma(x-1, y)
			sum, n = sum+max(d, -d), n+1
		}
	}
	return sum / float64(n)
}

// Width presets render through the one transform every rendering takes:
// Blur softens each width (the audit's PoC published Blur:30 byte-identical
// to Blur:0), and a Width×Height box shapes each width per Fit.
func TestWidthsBlurAndFit(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		c.Kinds = append(c.Kinds, media.Kind{Name: "poster",
			Uploads: []media.Upload{{Path: "poster", Types: stills, MaxBytes: 10 << 20}},
			Public: []media.Public{
				{Name: "sharp", From: "poster", To: "sharp-{w}.webp", Widths: []int{100, 200}},
				{Name: "blurred", From: "poster", To: "blurred-{w}.webp", Widths: []int{100, 200}, Image: media.Image{Blur: 30}},
				{Name: "cover", From: "poster", To: "cover-{w}.webp", Widths: []int{100, 200, 800},
					Image: media.Image{Width: 200, Height: 100, Fit: media.FitCover}},
				{Name: "inside", From: "poster", To: "inside-{w}.webp", Widths: []int{100, 200}, Image: media.Image{Width: 200, Height: 100}},
			}})
	})
	ref := e.ref(t, "poster", 1)
	e.put(t, ref, "poster.png", "image/png", banded(t))
	e.process(t, media.ProcessJob{Ref: ref})
	get := func(name string) []byte {
		t.Helper()
		b, _, ok := e.public(t, ref, name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		return b
	}
	for _, w := range []string{"100", "200"} {
		sharp, blurred := get("sharp-"+w+".webp"), get("blurred-"+w+".webp")
		if bytes.Equal(sharp, blurred) {
			t.Fatalf("width %s: Blur:30 is byte-identical to Blur:0", w)
		}
		if s, b := detail(t, sharp), detail(t, blurred); b > s/5 {
			t.Fatalf("width %s: blurred detail %.2f, sharp %.2f", w, b, s)
		}
	}
	// Cover fills each width's 2:1 box from the source's centre (the
	// checkerboard only); 800 is past the 400 px source, so it is 400 wide.
	for w, want := range map[string][2]int{"100": {100, 50}, "200": {200, 100}, "800": {400, 200}} {
		b := get("cover-" + w + ".webp")
		if gw, gh := webpSize(t, b); gw != want[0] || gh != want[1] {
			t.Fatalf("cover-%s is %dx%d, want %v", w, gw, gh, want)
		}
		img, _ := webp.Decode(bytes.NewReader(b))
		for _, y := range []int{1, want[1] - 2} {
			r, g, bl, _ := img.At(want[0]/2, y).RGBA()
			if c := (color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255}); near(c, red) || near(c, blue) {
				t.Fatalf("cover-%s row %d is a band (%v): not cropped to the centre", w, y, c)
			}
		}
	}
	// Inside keeps the whole square within the box: the bands survive.
	pixels(t, get("inside-100.webp"), 50, 50, map[[2]int]color.RGBA{{25, 3}: red, {25, 46}: blue})
	pixels(t, get("inside-200.webp"), 100, 100, map[[2]int]color.RGBA{{50, 5}: red, {50, 94}: blue})
}
