package image_test

import (
	"bytes"
	"context"
	stdimage "image"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
	"golang.org/x/image/bmp"

	"github.com/open-rails/contentkit/media"
)

// A stored original larger than its Upload's MaxBytes (the bucket holds
// other bytes than were committed) fails too_large without being read.
func TestOversizedSource(t *testing.T) {
	e := newEnv(t, func(c *media.Config) { c.Kinds[3].Uploads[0].MaxBytes = 4 << 10 })
	ref := e.ref(t, "post", 1)
	e.put(t, ref, "files/a.png", "image/png", solid(t, 10, 10, red))
	item, _ := e.reg.Item(ref)
	key, _ := item.Blob(e.file(t, ref, "files/a.png").Blob)
	big := bytes.Repeat([]byte{0x89}, 1<<20)
	if _, err := e.Store.Put(context.Background(), key, bytes.NewReader(big), int64(len(big)), media.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	s := &hooked{Store: e.Store}
	if err := e.processor(t, s).Process(context.Background(), media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if f := e.file(t, ref, "files/a.png").Fail(); f == nil || f.Code != media.CodeTooLarge {
		t.Fatalf("oversized source %+v", f)
	}
	if n := s.read.Load(); n >= 4<<10 {
		t.Fatalf("read %d bytes of an oversized source", n)
	}
}

// Only the formats' trusted libvips loaders run once the processor starts:
// SVG (librsvg) and BMP (ImageMagick) bytes neither load through libvips
// nor pass as a declared PNG.
func TestUntrustedFormats(t *testing.T) {
	e := newEnv(t, nil)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10" fill="red"/></svg>`)
	var b bytes.Buffer
	if err := bmp.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	untrusted := map[string][]byte{"svg": svg, "bmp": b.Bytes()}
	for name, typ := range map[string]vips.ImageType{"svg": vips.ImageTypeSVG, "bmp": vips.ImageTypeBMP} {
		src := untrusted[name]
		if vips.DetermineImageType(src) != typ {
			t.Fatalf("%s sniffs as %v", name, vips.DetermineImageType(src))
		}
		if !vips.IsTypeSupported(typ) {
			t.Logf("this libvips has no %s loader", name)
		}
		if img, err := vips.LoadImageFromBuffer(src, nil); err == nil {
			img.Close()
			t.Fatalf("libvips loaded %s after start", name)
		}
		if img, err := vips.NewThumbnailWithSizeFromBuffer(src, 5, 5, vips.InterestingNone, vips.SizeDown); err == nil {
			img.Close()
			t.Fatalf("libvips thumbnailed %s after start", name)
		}
	}
	if img, err := vips.LoadImageFromBuffer(solid(t, 4, 4, red), nil); err != nil {
		t.Fatalf("a PNG no longer loads: %v", err)
	} else {
		img.Close()
	}
	p := e.ref(t, "post", 1)
	for name, src := range untrusted {
		e.put(t, p, "files/"+name+".png", "image/png", src)
	}
	e.process(t, media.ProcessJob{Ref: p})
	for name := range untrusted {
		if f := e.file(t, p, "files/"+name+".png").Fail(); f == nil || f.Code != media.CodeImageUnreadable {
			t.Fatalf("%s declared a PNG: %+v", name, f)
		}
	}
}
