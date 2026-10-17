package image

import (
	"bytes"
	"fmt"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	_ "golang.org/x/image/webp"

	"github.com/open-rails/contentkit/media"
)

func sourceFixture(t *testing.T, typ string, width, height int) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 70, A: 255})
		}
	}
	var out bytes.Buffer
	var err error
	if typ == "image/jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 93})
	} else {
		err = png.Encode(&out, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	if typ == "image/webp" {
		data, _, err := encode(out.Bytes(), "image/png", media.Image{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return out.Bytes()
}

func TestSourceFormatCopiesAndBounds(t *testing.T) {
	if err := start(); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"image/png", "image/jpeg", "image/webp"} {
		for _, dims := range []media.Dims{{W: 32, H: 48}, {W: 12000, H: 24}, {W: 24, H: 12000}} {
			t.Run(fmt.Sprintf("%s/%dx%d", typ, dims.W, dims.H), func(t *testing.T) {
				src := sourceFixture(t, typ, dims.W, dims.H)
				before := bytes.Clone(src)
				spec := media.Image{Format: media.ImageFormatSource, Width: 6000, Height: 6000, Quality: 90}
				out, got, err := encode(src, typ, spec, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := dims
				if dims.W == 12000 {
					want = media.Dims{W: 6000, H: 12}
				} else if dims.H == 12000 {
					want = media.Dims{W: 12, H: 6000}
				}
				decoded, format, err := stdimage.DecodeConfig(bytes.NewReader(out))
				if err != nil || "image/"+format != typ || got != want || decoded.Width != want.W || decoded.Height != want.H {
					t.Fatalf("source format/bounds: format=%q metadata=%+v actual=%+v want=%+v err=%v", format, got, decoded, want, err)
				}
				if !bytes.Equal(before, src) {
					t.Fatal("encoding altered the archived source bytes")
				}
				if dims == want && !bytes.Equal(out, src) {
					t.Fatal("an under-limit source was re-encoded")
				}
				uncapped, originalDims, err := encode(src, typ, media.Image{Format: media.ImageFormatSource}, nil)
				if err != nil || originalDims != dims || !bytes.Equal(uncapped, src) {
					t.Fatalf("optional bound changed an uncapped source: %+v %v", originalDims, err)
				}
			})
		}
	}
}

func TestSourceFormatKeepsAnimations(t *testing.T) {
	if err := start(); err != nil {
		t.Fatal(err)
	}
	palette := color.Palette{color.Black, color.White}
	first := stdimage.NewPaletted(stdimage.Rect(0, 0, 120, 40), palette)
	second := stdimage.NewPaletted(first.Bounds(), palette)
	for i := range second.Pix {
		second.Pix[i] = 1
	}
	var src bytes.Buffer
	if err := gif.EncodeAll(&src, &gif.GIF{Image: []*stdimage.Paletted{first, second}, Delay: []int{7, 9}, LoopCount: 3}); err != nil {
		t.Fatal(err)
	}
	unchanged, _, err := encode(src.Bytes(), "image/gif", media.Image{Format: media.ImageFormatSource, Width: 6000, Height: 6000}, nil)
	if err != nil || !bytes.Equal(unchanged, src.Bytes()) {
		t.Fatalf("under-limit GIF changed: %v", err)
	}
	out, dims, err := encode(src.Bytes(), "image/gif", media.Image{Format: media.ImageFormatSource, Width: 60, Height: 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil || dims != (media.Dims{W: 60, H: 20}) || len(got.Image) != 2 || got.Delay[0] != 7 || got.Delay[1] != 9 || got.LoopCount != 3 {
		t.Fatalf("GIF animation was flattened or retimed: dims=%+v decoded=%+v err=%v", dims, got, err)
	}
	webpSource, _, err := encode(src.Bytes(), "image/gif", media.Image{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	webpBefore, err := decode(webpSource, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	webpLoops := webpBefore.Loop()
	webpBefore.Close()
	unchanged, _, err = encode(webpSource, "image/webp", media.Image{Format: media.ImageFormatSource, Width: 6000, Height: 6000}, nil)
	if err != nil || !bytes.Equal(unchanged, webpSource) {
		t.Fatalf("under-limit animated WebP changed: %v", err)
	}
	out, dims, err = encode(webpSource, "image/webp", media.Image{Format: media.ImageFormatSource, Width: 60, Height: 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	animated, err := decode(out, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	defer animated.Close()
	delays, _ := animated.PageDelay()
	if dims != (media.Dims{W: 60, H: 20}) || frames(animated) != 2 || len(delays) != 2 || delays[0] != 70 || delays[1] != 90 || animated.Loop() != webpLoops {
		t.Fatalf("WebP animation was flattened or retimed: dims=%+v frames=%d delays=%v loops=%d", dims, frames(animated), delays, animated.Loop())
	}
}
