package image_test

import (
	"bytes"
	"context"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/png"
	"slices"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
)

// Frame i is split down the middle: left[i] | right[i].
var (
	left  = []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 0, 255}}
	right = []color.RGBA{{0, 255, 255, 255}, {255, 0, 255, 255}, {255, 255, 255, 255}, {0, 0, 0, 255}}
)

func animKind(a media.Animation) media.Kind {
	return media.Kind{Name: "anim", Types: []string{"image/gif", "image/webp", "image/png"}, MaxBytes: 10 << 20, Animation: a,
		Specs: map[string]media.Spec{"large": {Width: 40, Quality: 90}},
		Slots: map[string]media.Slot{"cover": {Aspect: media.Aspect1x1, Widths: []int{20, 40}, Animation: a}}}
}

// animatedGIF is 4 frames of 80×80, 100-400 ms each, looping 3 times.
func animatedGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{}
	for i := range left {
		pal = append(pal, left[i], right[i])
	}
	g := &gif.GIF{LoopCount: 3}
	for i := range left {
		f := stdimage.NewPaletted(stdimage.Rect(0, 0, 80, 80), pal)
		for y := range 80 {
			for x := range 80 {
				c := left[i]
				if x >= 40 {
					c = right[i]
				}
				f.Set(x, y, c)
			}
		}
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 10*(i+1))
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// animation decodes every frame of an image: its size per frame, delays, loop and each frame's centre colour.
type animation struct {
	w, h, frames int
	delays       []int
	loop         int
	centres      []color.RGBA
}

func decodeAnimation(t *testing.T, b []byte) animation {
	t.Helper()
	p := vips.NewImportParams()
	p.NumPages.Set(-1)
	img, err := vips.LoadImageFromBuffer(b, p)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	a := animation{w: img.Width(), h: img.PageHeight(), loop: img.Loop()}
	a.frames = img.Height() / a.h
	a.delays, _ = img.PageDelay()
	strip, _, err := img.ExportPng(vips.NewPngExportParams())
	if err != nil {
		t.Fatal(err)
	}
	s, err := png.Decode(bytes.NewReader(strip))
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.frames {
		r, g, bl, _ := s.At(a.w/2, i*a.h+a.h/2).RGBA()
		a.centres = append(a.centres, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255})
	}
	return a
}

func near(a, b color.RGBA) bool {
	d := func(x, y uint8) int { return max(int(x)-int(y), int(y)-int(x)) }
	return d(a.R, b.R) < 24 && d(a.G, b.G) < 24 && d(a.B, b.B) < 24
}

// An animated GIF and an animated WebP stay animated through a crop, a
// half-turn and a resize: every frame is cut at the same place, frames keep
// their order, delays and loop count.
func TestAnimatedRenditionsKeepEveryFrame(t *testing.T) {
	e := newEnv(t, animKind(media.AnimationAllow))
	src := animatedGIF(t)
	in := decodeAnimation(t, src)
	if in.frames != 4 {
		t.Fatalf("fixture: %+v", in)
	}
	webpSrc := func() []byte {
		p := vips.NewImportParams()
		p.NumPages.Set(-1)
		img, err := vips.LoadImageFromBuffer(src, p)
		if err != nil {
			t.Fatal(err)
		}
		defer img.Close()
		b, _, err := img.ExportWebp(&vips.WebpExportParams{Quality: 95, Lossless: true})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}()

	ref := contentref.New(e.Tenant, "anim", cid(1))
	crop := &media.Edit{Crop: &media.Crop{X: 40, Y: 0, W: 40, H: 80}, Rotate: 180}
	gifName := e.uploadAs(t, ref, "", "image/gif", src)
	webpName := e.uploadAs(t, ref, "", "image/webp", webpSrc)
	e.commit(t, ref, ins("a.gif", gifName), ins("b.webp", webpName), media.Op{Op: media.OpEdit, Name: "a.gif", Edit: crop})
	e.drain(t)
	m, _ := e.manifest(t, ref)
	for i, want := range [][]color.RGBA{right, nil} {
		f := m.Files[i]
		if f.Failed() != nil || f.Variants["large"].Blob == "" {
			t.Fatalf("%s: %+v", f.Name, f)
		}
		b, _ := e.blob(t, ref, f.Variants["large"].Blob)
		out := decodeAnimation(t, b)
		if out.frames != 4 || !slices.Equal(out.delays, in.delays) || out.loop != in.loop {
			t.Fatalf("%s: %d frames, delays %v loop %d; want 4, %v, %d", f.Name, out.frames, out.delays, out.loop, in.delays, in.loop)
		}
		if want == nil {
			if out.w != 40 || out.h != 40 {
				t.Fatalf("%s: %dx%d", f.Name, out.w, out.h)
			}
			continue
		}
		if out.w != 40 || out.h != 80 {
			t.Fatalf("%s: %dx%d per frame", f.Name, out.w, out.h)
		}
		for j, c := range out.centres {
			if !near(c, want[j]) {
				t.Fatalf("%s frame %d: %v, want %v (frames %v)", f.Name, j, c, want[j], out.centres)
			}
		}
	}

	// A slot keeps the animation too, at every width.
	e.slot(t, ref, "cover", src, &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 40, H: 40}})
	e.drain(t)
	if sm := e.slotManifest(t, ref, "cover"); sm.Error != "" || len(sm.Outputs) != 2 {
		t.Fatalf("cover: %+v", sm)
	}
	for _, w := range []int{20, 40} {
		b, _ := e.object(t, e.slotOutput(t, ref, "cover", w))
		out := decodeAnimation(t, b)
		if out.frames != 4 || !slices.Equal(out.delays, in.delays) || !near(out.centres[2], left[2]) {
			t.Fatalf("cover %d: %+v", w, out)
		}
	}
}

// AnimationReject refuses an animated upload, typed and recorded, with nothing written; a still passes.
func TestAnimationRejectRefuses(t *testing.T) {
	e := newEnv(t, animKind(media.AnimationReject))
	ref := contentref.New(e.Tenant, "anim", cid(2))
	e.commit(t, ref, ins("a.gif", e.uploadAs(t, ref, "", "image/gif", animatedGIF(t))), ins("b.png", e.upload(t, ref, "", pngImage(t, 64, 64, 1))))
	e.slot(t, ref, "cover", animatedGIF(t), nil)
	e.drain(t)
	m, _ := e.manifest(t, ref)
	if ff := m.Files[0].Failed(); ff == nil || ff.Code != media.CodeAnimationNotAllowed || len(m.Files[0].Variants) != 0 {
		t.Fatalf("animated file: %+v", m.Files[0])
	}
	if m.Files[1].Failed() != nil || m.Files[1].Variants["large"].Blob == "" {
		t.Fatalf("still file: %+v", m.Files[1])
	}
	sm := e.slotManifest(t, ref, "cover")
	if sm.ErrorCode != media.CodeAnimationNotAllowed || len(sm.Outputs) != 0 || sm.Animation != media.AnimationReject {
		t.Fatalf("cover: %+v", sm)
	}
	for _, w := range []int{20, 40} {
		if k := e.slotOutput(t, ref, "cover", w); k != "" {
			t.Fatalf("cover %d written for a refused animation", w)
		}
	}
	// A refused file is not retried until its source or edit changes.
	e.queue.jobs = []media.ProcessJob{{Ref: ref}}
	failed := len(e.failed)
	e.drain(t)
	if len(e.failed) != failed {
		t.Fatalf("refusal retried: %v", e.failed)
	}
}

// Frames and pixels over all frames are bounded, so an animation cannot be a decompression bomb.
func TestAnimationLimits(t *testing.T) {
	for name, c := range map[string]struct {
		cfg  image.Config
		code string
	}{
		"frames": {image.Config{MaxFrames: 3}, media.CodeAnimationTooLong},
		"pixels": {image.Config{MaxPixels: 3 * 80 * 80}, media.CodeImageTooLarge},
		"time":   {image.Config{MaxAnimationSeconds: 0.5}, media.CodeAnimationTooLong},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, animKind(media.AnimationAllow))
			cfg := c.cfg
			cfg.Store, cfg.Kinds, cfg.Manifests = e.store, e.kinds, e.manifests
			proc, err := image.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ref := contentref.New(e.Tenant, "anim", cid(3))
			e.commit(t, ref, ins("a.gif", e.uploadAs(t, ref, "", "image/gif", animatedGIF(t))))
			for _, j := range e.queue.take() {
				if err := proc.Process(context.Background(), j); err != nil {
					t.Fatal(err)
				}
			}
			m, _ := e.manifest(t, ref)
			if ff := m.Files[0].Failed(); ff == nil || ff.Code != c.code || ff.Details == nil || len(m.Files[0].Variants) != 0 {
				t.Fatalf("%+v", m.Files[0].Failure)
			}
		})
	}
}

// AVIF decodes (libheif with an AV1 decoder); bytes that are not AVIF under image/avif are refused as unreadable.
func TestAVIF(t *testing.T) {
	still, err := vips.NewImageFromBuffer(pngImage(t, 64, 64, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer still.Close()
	avif, _, err := still.ExportAvif(vips.NewAvifExportParams())
	if err != nil {
		t.Fatalf("AVIF encoder (libheif-plugin-aomenc): %v", err)
	}
	k := animKind(media.AnimationAllow)
	k.Types = append(k.Types, "image/avif")
	e := newEnv(t, k)
	ref := contentref.New(e.Tenant, "anim", cid(4))
	e.commit(t, ref, ins("a.avif", e.uploadAs(t, ref, "", "image/avif", avif)), ins("b.avif", e.uploadAs(t, ref, "", "image/avif", pngImage(t, 8, 8, 1))))
	e.drain(t)
	m, _ := e.manifest(t, ref)
	if m.Files[0].Failed() != nil || m.Files[0].Variants["large"].Blob == "" {
		t.Fatalf("avif: %+v", m.Files[0])
	}
	if ff := m.Files[1].Failed(); ff == nil || ff.Code != media.CodeImageUnreadable || ff.Details.Type != "image/avif" {
		t.Fatalf("fake avif: %+v", m.Files[1].Failure)
	}
}
