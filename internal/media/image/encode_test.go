package image

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
)

func TestMain(m *testing.M) {
	if err := start(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

var limits = rules{maxPixels: 100_000_000, maxFrames: 1000, maxSeconds: 60}

// halves is a w×h image, its left half red and its right half blue.
func halves(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{255, 0, 0, 255}
			if x >= w/2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func pngOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func dimsOf(t *testing.T, b []byte) (int, int, int) {
	t.Helper()
	p := vips.NewImportParams()
	p.NumPages.Set(-1)
	img, err := vips.LoadImageFromBuffer(b, p)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	return img.Width(), img.PageHeight(), frames(img)
}

func probed(t *testing.T, src []byte, typ string) source {
	t.Helper()
	info, err := probe(src, typ, limits)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// A cover box larger than its source is the source's largest centred crop
// at the box's aspect, never an enlargement, as media.Image documents.
func TestCoverNeverEnlarges(t *testing.T) {
	src := pngOf(t, halves(40, 20))
	cover := media.Image{Width: 192, Height: 108, Fit: media.FitCover}
	out, d, err := encode(src, "image/png", probed(t, src, "image/png"), cover, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := dimsOf(t, out); d != (media.Dims{W: 36, H: 20}) || w != 36 || h != 20 {
		t.Fatalf("cover of 40x20 into 192x108 rendered %dx%d (recorded %+v), want the 36x20 centred 16:9 crop", w, h, d)
	}
	// A source at least the box still fills it exactly.
	big := pngOf(t, halves(400, 400))
	if _, d, err = encode(big, "image/png", probed(t, big, "image/png"), cover, nil, limits); err != nil || d != (media.Dims{W: 192, H: 108}) {
		t.Fatalf("cover of 400x400 rendered %+v, %v", d, err)
	}
	// Widths presets use the same geometry for every rung.
	p := &media.Public{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{64, 320}, Image: media.Image{Width: 16, Height: 9, Fit: media.FitCover}}
	outs, _, err := encodePublic(src, "image/png", p, []string{"cover-64.webp", "cover-320.webp"}, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range outs {
		if r.dims.W > 40 || r.dims.H > 20 {
			t.Fatalf("%s enlarged the 40x20 source to %+v", name, r.dims)
		}
	}
}

// An animation through a large cover renders at most its own pixels: the
// output budget holds, and one over it is refused before decoding.
func TestAnimatedCoverStaysWithinBudget(t *testing.T) {
	palette := []color.Color{color.Black, color.White}
	anim := &gif.GIF{}
	for i := range 50 {
		frame := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
		frame.SetColorIndex(i%4, 1+i%2, 1) // consecutive frames differ in the centre rows
		anim.Image, anim.Delay = append(anim.Image, frame), append(anim.Delay, 2)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, anim); err != nil {
		t.Fatal(err)
	}
	info := probed(t, b.Bytes(), "image/gif")
	cover := media.Image{Width: 1920, Height: 1080, Fit: media.FitCover}
	out, _, err := encode(b.Bytes(), "image/gif", info, cover, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, n := dimsOf(t, out); n != 50 || w*h > 16 {
		t.Fatalf("50-frame 4x4 animation through a 1920x1080 cover rendered %d frames of %dx%d", n, w, h)
	}
	if err := (rules{maxPixels: 1000}).output(info, 10, 10); !isPermanent(err) {
		t.Fatalf("50 frames of 10x10 passed a 1000-pixel output budget: %v", err)
	}
}

// An upload's MaxPixels lowers the processor's bound; the avatar sets one.
func TestUploadMaxPixels(t *testing.T) {
	p := &Processor{c: Config{MaxPixels: 100_000_000, MaxFrames: 1000, MaxAnimationSeconds: 60}}
	src := pngOf(t, halves(40, 40))
	_, err := probe(src, "image/png", p.rules(media.Upload{MaxPixels: 1000}, media.AnimationAllow))
	var ie *media.ImageError
	if !errors.As(err, &ie) || ie.Code != media.CodeImageTooLarge || ie.Details.MaxPixels != 1000 {
		t.Fatalf("a 1600-pixel image passed an upload's 1000-pixel bound: %v", err)
	}
	if got := p.rules(media.Upload{MaxPixels: 200_000_000}, media.AnimationAllow).maxPixels; got != 100_000_000 {
		t.Fatalf("an upload raised the processor's bound to %d", got)
	}
	if got := p.rules(avatar.User().Uploads[0], media.AnimationAllow).maxPixels; got != 25_000_000 {
		t.Fatalf("avatars decode up to %d pixels, want 25 MP", got)
	}
}

// A large still decodes at a fraction for small renditions, its edit scaled:
// the avatar's rungs come out exact and show the cropped half.
func TestShrinkOnLoad(t *testing.T) {
	src := jpegOf(t, halves(2400, 1200))
	info := probed(t, src, "image/jpeg")
	right := &media.Edit{Crop: &media.Crop{X: 1200, Y: 0, W: 1200, H: 1200}}
	img, scaled, err := load(src, "image/jpeg", info, right, media.Dims{W: 512, H: 512})
	if err != nil {
		t.Fatal(err)
	}
	if img.Width() > 1200 || scaled.Crop.W < 512 || scaled.Crop.X+scaled.Crop.W > img.Width() {
		t.Fatalf("2400x1200 for 512 px loaded at %dx%d with crop %+v", img.Width(), img.Height(), *scaled.Crop)
	}
	img.Close()
	p := &avatar.User().Public[0]
	names := []string{"avatar-64.webp", "avatar-128.webp", "avatar-256.webp", "avatar-512.webp"}
	outs, _, err := encodePublic(src, "image/jpeg", p, names, right, limits)
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range p.Widths {
		r := outs[names[i]]
		if r.dims != (media.Dims{W: w, H: w}) {
			t.Fatalf("%s is %+v, want %dx%d", names[i], r.dims, w, w)
		}
		out, err := vips.NewImageFromBuffer(r.webp)
		if err != nil {
			t.Fatal(err)
		}
		px, err := out.GetPoint(w/2, w/2)
		out.Close()
		if err != nil || len(px) < 3 || px[2] < 200 || px[0] > 60 {
			t.Fatalf("%s centre is %v, want the cropped blue half", names[i], px)
		}
	}
}

// A path the kind no longer declares is refused as such, not as too large.
func TestUndeclaredUploadPath(t *testing.T) {
	reg, err := mediastore.NewRegistry(media.Config{Namespace: "t", Kinds: []media.Kind{{Name: "gallery",
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}}}}}, mediastore.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	k, err := reg.Kind("gallery")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Processor{}).read(context.Background(), "unused", media.File{Path: "covers/old.png", Type: "image/png"}, k)
	var ie *media.ImageError
	if !isPermanent(err) || !errors.As(err, &ie) || ie.Code != media.CodeNotFound || !bytes.Contains([]byte(ie.Message), []byte("no longer a declared upload path")) {
		t.Fatalf("an undeclared path read as %v", err)
	}
}
