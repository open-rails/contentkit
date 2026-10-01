package image_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
)

// Image presets render each page through its edit, record provenance and
// clear pending; the zip packs the high files in manifest order and is
// rebuilt when they move; a second pass redoes nothing.
func TestPrivatePresetsAndZip(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	for i, c := range []color.RGBA{red, green, blue} {
		e.put(t, g, fmt.Sprintf("originals/%d.png", i+1), "image/png", solid(t, 300, 400, c))
	}
	e.commit(t, g, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "Book"}})
	e.process(t, media.ProcessJob{Ref: g})
	m := e.manifest(t, g)
	k, _ := e.reg.Kind("gallery")
	if r := k.Readiness(m); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	th := e.file(t, g, "thumb/2.webp")
	if th.From != "originals/2.png" || th.Preset != "thumb" || th.FP == "" || th.W != 100 || th.H != 150 {
		t.Fatalf("thumb %+v", th)
	}
	pixels(t, e.blob(t, g, th), 100, 150, map[[2]int]color.RGBA{{50, 75}: green})
	if up := e.file(t, g, "originals/1.png"); up.W != 300 || up.H != 400 || up.Pending != nil {
		t.Fatalf("measured upload %+v", up)
	}
	z := e.file(t, g, "download/pages.zip")
	if z.Download != "Book.zip" || z.From != "high/" || z.FP != media.ZipFP(k.ZipInputs(m, &k.Private[2])) {
		t.Fatalf("zip %+v", z)
	}
	names := func(b []byte) []string {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range zr.File {
			out = append(out, f.Name)
		}
		return out
	}
	if got := names(e.blob(t, g, z)); !slices.Equal(got, []string{"1.webp", "2.webp", "3.webp"}) {
		t.Fatalf("zip entries %v", got)
	}
	before := e.manifest(t, g)
	e.process(t, media.ProcessJob{Ref: g})
	if after := e.manifest(t, g); !slices.EqualFunc(before.Files, after.Files, func(a, b media.File) bool { return a.Blob == b.Blob && a.Path == b.Path }) {
		t.Fatal("a second pass changed outputs")
	}
	e.commit(t, g, media.Op{Op: media.OpMove, Path: "originals/3.png", Index: new(int)})
	e.process(t, media.ProcessJob{Ref: g})
	if got := names(e.blob(t, g, e.file(t, g, "download/pages.zip"))); !slices.Equal(got, []string{"3.webp", "1.webp", "2.webp"}) {
		t.Fatalf("zip after a move %v", got)
	}
}

// A deploy that changes a preset's spec makes exactly that preset's outputs
// stale; Force redoes current ones; Preset limits a job.
func TestSpecChangeAndForce(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	e.put(t, g, "originals/1.png", "image/png", solid(t, 300, 400, red))
	e.process(t, media.ProcessJob{Ref: g})
	high := e.file(t, g, "high/1.webp")
	thumb := e.file(t, g, "thumb/1.webp")
	e.deploy(t, func(c *media.Config) {
		c.Kinds[0].Private[0].Image = &media.Image{Width: 50, Height: 50, Fit: media.FitCover}
	})
	e.process(t, media.ProcessJob{Ref: g})
	if th := e.file(t, g, "thumb/1.webp"); th.FP == thumb.FP || th.W != 50 {
		t.Fatalf("thumb not regenerated %+v", th)
	}
	if h := e.file(t, g, "high/1.webp"); h.FP != high.FP || h.Blob != high.Blob {
		t.Fatalf("high regenerated %+v", h)
	}
	if _, err := e.ms.EditExisting(context.Background(), g, func(m *media.Manifest) error {
		m.Files[m.Find("high/1.webp")].Blob = thumb.Blob // a wrong blob a producer fix must redo
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: g, Preset: "thumb", Force: true})
	if h := e.file(t, g, "high/1.webp"); h.Blob != thumb.Blob {
		t.Fatal("Preset thumb touched high")
	}
	e.process(t, media.ProcessJob{Ref: g, Preset: "high", Force: true})
	if h := e.file(t, g, "high/1.webp"); h.Blob != high.Blob {
		t.Fatalf("Force did not redo high: %+v", h)
	}
}

// Public presets render fixed names through the crop at the preset's
// aspect, never upscaled, with their provenance as object metadata, and are
// purged on every write; a removed upload's names are deleted and purged; a
// hidden item renders none.
func TestPublicPreset(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	e.put(t, g, "cover.png", "image/png", quadrants(t), media.Op{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 400, H: 1}}})
	e.process(t, media.ProcessJob{Ref: g})
	c := e.file(t, g, "cover.png")
	if c.Pending != nil || c.Edit.Crop.H != 133 || c.W != 400 || c.H != 200 {
		t.Fatalf("cover %+v", c)
	}
	for _, w := range []int{150, 300} {
		b, obj, ok := e.public(t, g, fmt.Sprintf("cover-%d.webp", w))
		if !ok || obj.Metadata["fp"] == "" || obj.Metadata["from"] != "cover.png" {
			t.Fatalf("cover-%d: %v %+v", w, ok, obj.Metadata)
		}
		pixels(t, b, w, w/3, map[[2]int]color.RGBA{{w / 4, w / 12}: red, {3 * w / 4, w / 12}: blue})
	}
	// 600 is wider than the 400px edit: rendered at the edited width.
	if b, _, ok := e.public(t, g, "cover-600.webp"); !ok {
		t.Fatal("cover-600 missing")
	} else if w, h := webpSize(t, b); w != 400 || h != 133 {
		t.Fatalf("cover-600 is %dx%d", w, h)
	}
	if p := e.takePurged(); len(p) != 3 {
		t.Fatalf("purged %v", p)
	}
	e.process(t, media.ProcessJob{Ref: g})
	if p := e.takePurged(); len(p) != 0 {
		t.Fatalf("an unchanged cover was rewritten: %v", p)
	}
	e.commit(t, g, media.Op{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 200, Y: 100, W: 200, H: 1}}})
	e.process(t, media.ProcessJob{Ref: g})
	b, _, _ := e.public(t, g, "cover-150.webp")
	pixels(t, b, 150, 50, map[[2]int]color.RGBA{{75, 25}: white})
	if p := e.takePurged(); len(p) != 3 {
		t.Fatalf("re-crop purged %v", p)
	}
	if _, err := e.up.Commit(context.Background(), e.editor, g, []media.Op{{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 50, H: 1}}}}); err == nil {
		t.Fatal("an edit under MinWidth accepted")
	}
	e.commit(t, g, media.Op{Op: media.OpRemove, Path: "cover.png"})
	e.process(t, media.ProcessJob{Ref: g})
	if _, _, ok := e.public(t, g, "cover-150.webp"); ok {
		t.Fatal("a removed cover's public name kept")
	}
	if p := e.takePurged(); len(p) != 3 {
		t.Fatalf("removal purged %v", p)
	}
	// A hidden item renders nothing public.
	h := e.ref(t, "gallery", 2)
	e.put(t, h, "cover.png", "image/png", quadrants(t))
	if _, err := e.ms.EditExisting(context.Background(), h, func(m *media.Manifest) error {
		m.Hidden = true
		m.Files[m.Find("cover.png")].Pending = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: h})
	if _, _, ok := e.public(t, h, "cover-150.webp"); ok {
		t.Fatal("a hidden item rendered its cover")
	}
}

// EXIF orientation is applied before measuring and editing.
func TestOrientation(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	img := paint(200, 100, func(x, _ int) color.RGBA {
		if x < 100 {
			return red
		}
		return blue
	})
	e.put(t, g, "originals/1.jpg", "image/jpeg", orientedJPEG(t, img, 6)) // 90° clockwise: displayed 100×200
	e.process(t, media.ProcessJob{Ref: g})
	if up := e.file(t, g, "originals/1.jpg"); up.W != 100 || up.H != 200 {
		t.Fatalf("oriented size %dx%d", up.W, up.H)
	}
	pixels(t, e.blob(t, g, e.file(t, g, "high/1.webp")), 100, 200, map[[2]int]color.RGBA{{50, 50}: red, {50, 150}: blue})
}

// Animations stay animated where allowed; a refusing preset, an
// undecodable file or bytes that do not match their hash fail the upload
// for its blob (Hooks.Failed), and the item reads as failed.
func TestAnimationAndFailures(t *testing.T) {
	e := newEnv(t, nil)
	a := e.ref(t, "anim", 1)
	e.put(t, a, "files/a.gif", "image/gif", animatedGIF(t))
	e.process(t, media.ProcessJob{Ref: a})
	w, h, n, delays := frames(t, e.blob(t, a, e.file(t, a, "large/a.webp")))
	if w != 40 || h != 40 || n != 4 || len(delays) != 4 || delays[3] != 400 {
		t.Fatalf("animated output %dx%d ×%d %v", w, h, n, delays)
	}
	u := e.ref(t, "user", 1)
	e.put(t, u, "avatar", "image/gif", animatedGIF(t))
	e.process(t, media.ProcessJob{Ref: u})
	if f := e.file(t, u, "avatar.gif").Fail(); f == nil || f.Code != media.CodeAnimationNotAllowed {
		t.Fatalf("animated avatar failure %+v", f)
	}
	k, _ := e.reg.Kind("user")
	if r := k.Readiness(e.manifest(t, u)); r.State != media.StateFailed {
		t.Fatalf("readiness %+v", r)
	}
	p := e.ref(t, "post", 1)
	e.put(t, p, "files/bad.png", "image/png", []byte("not a png at all"))
	e.process(t, media.ProcessJob{Ref: p})
	if f := e.file(t, p, "files/bad.png").Fail(); f == nil || f.Code != media.CodeImageUnreadable {
		t.Fatalf("undecodable %+v", f)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, want := range []string{"avatar.gif", "files/bad.png"} {
		if !slices.Contains(e.failed, want) {
			t.Fatalf("Hooks.Failed %v lacks %s", e.failed, want)
		}
	}
}

// Choose picks a per-file spec; KeepOriginals false drops an upload's blob
// once its outputs exist; editor views render on request.
func TestChooseGoneAndEditorViews(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		post := &c.Kinds[3]
		post.Private[0].Choose = func(f media.File) *media.Image {
			if strings.Contains(f.Path, "tall") {
				return &media.Image{Width: 20, Height: 80}
			}
			return nil
		}
	})
	p := e.ref(t, "post", 1)
	e.put(t, p, "files/tall.png", "image/png", solid(t, 100, 400, red))
	e.put(t, p, "files/wide.png", "image/png", solid(t, 400, 200, red))
	e.process(t, media.ProcessJob{Ref: p, Editor: true})
	if w := e.file(t, p, "web/tall.webp"); w.W != 20 || w.H != 80 {
		t.Fatalf("chosen spec %dx%d", w.W, w.H)
	}
	if w := e.file(t, p, "web/wide.webp"); w.W != 50 || w.H != 25 {
		t.Fatalf("default spec %dx%d", w.W, w.H)
	}
	up := e.file(t, p, "files/wide.png")
	if !up.Gone {
		t.Fatalf("an upload of a kind without KeepOriginals kept: %+v", up)
	}
	if slices.Contains(e.manifest(t, p).Blobs(), up.Blob) {
		t.Fatal("a gone upload's blob is still referenced")
	}
	g := e.ref(t, "gallery", 1)
	e.put(t, g, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: g, Editor: true})
	view := e.reg.EditorView(e.file(t, g, "cover.png"))
	item, _ := e.reg.Item(g)
	key, _ := item.Blob(view)
	b, _ := e.read(t, key)
	if w, h := webpSize(t, b); w != 400 || h != 200 {
		t.Fatalf("editor view %dx%d", w, h)
	}
}

// PublishDefaults renders each public default to its kind's _default item
// at every width, once.
func TestPublishDefaults(t *testing.T) {
	e := newEnv(t, nil)
	e.deploy(t, func(c *media.Config) {
		c.Kinds[0].Defaults = fstest.MapFS{"cover.png": {Data: quadrants(t)}}
	})
	ctx := context.Background()
	keys, err := image.PublishDefaults(ctx, e.Store, e.reg)
	if err != nil || len(keys) != 3 {
		t.Fatalf("published %v %v", keys, err)
	}
	k, _ := e.reg.Kind("gallery")
	b, obj := e.read(t, k.DefaultKey("cover-300.webp"))
	if w, h := webpSize(t, b); w != 300 || h != 100 || obj.Metadata["fp"] == "" {
		t.Fatalf("default %dx%d %+v", w, h, obj.Metadata)
	}
	if keys, err := image.PublishDefaults(ctx, e.Store, e.reg); err != nil || len(keys) != 0 {
		t.Fatalf("republished %v %v", keys, err)
	}
}
