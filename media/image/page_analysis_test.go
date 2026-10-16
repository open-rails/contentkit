package image

import (
	"bytes"
	"context"
	"errors"
	stdimage "image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func analyzeFixture(t *testing.T, src []byte, typ string) media.PageAnalysis {
	t.Helper()
	a, err := analyzePage(context.Background(), src, typ, rules{maxPixels: 100_000_000, maxFrames: 1000, maxSeconds: 60, animation: media.AnimationAllow})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func analysisPNG(t *testing.T, img stdimage.Image, compression png.CompressionLevel) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: compression}).Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func analysisWhite(w, h int) *stdimage.NRGBA {
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &stdimage.Uniform{C: color.White}, stdimage.Point{}, draw.Src)
	return img
}

func TestPageAnalysisBlankAndSparseMarks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mark  *color.NRGBA
		blank string
	}{
		{name: "white", blank: "blank"},
		{name: "single black pixel", mark: &color.NRGBA{A: 255}, blank: "suspected"},
		{name: "faint mark", mark: &color.NRGBA{R: 253, G: 253, B: 253, A: 255}, blank: "suspected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := analysisWhite(1280, 700) // spans multiple bounded stripes
			if tc.mark != nil {
				img.SetNRGBA(1279, 699, *tc.mark)
			}
			a := analyzeFixture(t, analysisPNG(t, img, png.DefaultCompression), "image/png")
			if a.Status != "complete" || a.Blank != tc.blank || a.Metrics.Pixels != 1280*700 || a.PixelSHA256 == "" {
				t.Fatalf("analysis: %+v", a)
			}
			if tc.mark != nil && a.Confidence != "possible" {
				t.Fatalf("sparse content must remain a suggestion: %+v", a)
			}
		})
	}
	black := stdimage.NewGray(stdimage.Rect(0, 0, 20, 20))
	if a := analyzeFixture(t, analysisPNG(t, black, png.DefaultCompression), "image/png"); a.Blank != "suspected" {
		t.Fatalf("uniform black must not be auto-excluded: %+v", a)
	}
}

func TestPageAnalysisCanonicalPixels(t *testing.T) {
	img := analysisWhite(32, 16)
	img.SetNRGBA(3, 9, color.NRGBA{R: 20, G: 150, B: 60, A: 127})
	a := analyzeFixture(t, analysisPNG(t, img, png.DefaultCompression), "image/png")
	b := analyzeFixture(t, analysisPNG(t, img, png.NoCompression), "image/png")
	if a.PixelSHA256 != b.PixelSHA256 || a.SourceSHA256 == b.SourceSHA256 {
		t.Fatalf("same pixels with different encodings: %+v / %+v", a, b)
	}
	img.SetNRGBA(3, 9, color.NRGBA{R: 20, G: 151, B: 60, A: 127})
	if c := analyzeFixture(t, analysisPNG(t, img, png.DefaultCompression), "image/png"); c.PixelSHA256 == a.PixelSHA256 {
		t.Fatal("a visible pixel change must change the fingerprint")
	}
	clear := stdimage.NewNRGBA(stdimage.Rect(0, 0, 32, 16))
	a = analyzeFixture(t, analysisPNG(t, clear, png.NoCompression), "image/png")
	for i := 0; i < len(clear.Pix); i += 4 {
		clear.Pix[i], clear.Pix[i+1], clear.Pix[i+2] = 90, 20, 255
	}
	b = analyzeFixture(t, analysisPNG(t, clear, png.NoCompression), "image/png")
	if a.Blank != "blank" || a.BlankReason != "fully_transparent" || a.Confidence != "exact" || a.PixelSHA256 != b.PixelSHA256 {
		t.Fatalf("hidden RGB must not matter: %+v / %+v", a, b)
	}
}

func TestPageAnalysisUsesUprightPixels(t *testing.T) {
	img := analysisWhite(40, 20)
	draw.Draw(img, stdimage.Rect(0, 0, 20, 10), &stdimage.Uniform{C: color.Black}, stdimage.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	withOrientation := func(orientation byte) []byte {
		tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, orientation, 0, 0, 0, 0, 0, 0}
		payload := append([]byte("Exif\x00\x00"), tiff...)
		out := append([]byte{}, buf.Bytes()[:2]...)
		out = append(out, 0xff, 0xe1, byte((len(payload)+2)>>8), byte(len(payload)+2))
		out = append(out, payload...)
		return append(out, buf.Bytes()[2:]...)
	}
	a := analyzeFixture(t, withOrientation(6), "image/jpeg")
	b := analyzeFixture(t, withOrientation(8), "image/jpeg")
	if a.Status != "complete" || a.Width != 20 || a.Height != 40 || b.Width != 20 || b.Height != 40 || a.PixelSHA256 == b.PixelSHA256 {
		t.Fatalf("orientation must affect displayed pixels: %+v / %+v", a, b)
	}
}

func TestPageAnalysisUnsupportedAndFailureAreEvidence(t *testing.T) {
	img16 := stdimage.NewGray16(stdimage.Rect(0, 0, 10, 10))
	if a := analyzeFixture(t, analysisPNG(t, img16, png.NoCompression), "image/png"); a.Status != "unsupported" || a.Blank != "unknown" || a.PixelSHA256 != "" {
		t.Fatalf("16-bit sources must not be downcast for duplicate decisions: %+v", a)
	}
	frame := stdimage.NewPaletted(stdimage.Rect(0, 0, 10, 10), color.Palette{color.White, color.Black})
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*stdimage.Paletted{frame, frame}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	if a := analyzeFixture(t, b.Bytes(), "image/gif"); a.Status != "unsupported" || a.Blank != "unknown" {
		t.Fatalf("animation must be retained: %+v", a)
	}
	if a := analyzeFixture(t, []byte("not an image"), "image/png"); a.Status != "failed" || a.Blank != "unknown" || len(a.Flags) == 0 {
		t.Fatalf("decode failure must remain terminal evidence: %+v", a)
	}
	src := analysisPNG(t, analysisWhite(10, 10), png.DefaultCompression)
	a, err := analyzePage(context.Background(), src, "image/png", rules{maxPixels: 99, animation: media.AnimationAllow})
	if err != nil || a.Status != "failed" || a.PixelSHA256 != "" {
		t.Fatalf("source bounds: %+v %v", a, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := analyzePage(ctx, src, "image/png", rules{maxPixels: 100}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must be retryable: %v", err)
	}
}
