package image_test

import (
	"context"
	"encoding/json"
	"image/color"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
)

func TestPageAnalysisWorkerPreservesOverrides(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		c.Kinds[0].Versions = true
		c.Kinds[0].Publish = &media.Publish{Pages: "originals/{name}", Renditions: []string{"high", "thumb"}}
	})
	var err error
	e.proc, err = image.New(image.Config{Store: e.Store, Manifests: e.ms, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := e.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	src := encodePNG(t, paint(200, 200, func(x, y int) color.RGBA {
		if x < 20 && y < 20 {
			return color.RGBA{A: 255}
		}
		return white
	}))
	e.put(t, ref, "originals/007.png", "image/png", src,
		media.Op{Op: media.OpEdit, Path: "originals/007.png", Edit: &media.Edit{Crop: &media.Crop{X: 50, Y: 50, W: 100, H: 100}}})
	e.put(t, ref, "cover.png", "image/png", solid(t, 200, 200, red))
	original := e.file(t, ref, "originals/007.png")
	ctx := context.Background()
	_, err = e.ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		m.Files[m.Find(original.Path)].Meta = map[string]any{"page": map[string]any{"name": "007", "mode": "include", "revision": 4}, "external": "keep"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: ref})
	f := e.file(t, ref, original.Path)
	a, ok := media.ReadPageAnalysis(f.Meta)
	if !ok || !f.HasPageAnalysis(media.PageAnalysisRecipe) || a.Blank != "none" || a.Width != 200 || a.Height != 200 {
		t.Fatalf("analysis must describe original, not the blank crop: %+v", a)
	}
	page := f.Meta["page"].(map[string]any)
	revision, _ := json.Marshal(page["revision"])
	if page["mode"] != "include" || page["name"] != "007" || string(revision) != "4" || f.Meta["external"] != "keep" {
		t.Fatalf("admin metadata changed: %+v", f.Meta)
	}
	if f.Blob != original.Blob || !reflect.DeepEqual(e.blob(t, ref, f), src) {
		t.Fatal("analysis changed the original")
	}
	if _, ok := media.ReadPageAnalysis(e.file(t, ref, "cover.png").Meta); ok {
		t.Fatal("cover is not a page")
	}
	before, _ := json.Marshal(f.Meta)
	e.process(t, media.ProcessJob{Ref: ref, Force: true, Preset: "thumb"})
	after, _ := json.Marshal(e.file(t, ref, original.Path).Meta)
	if string(before) != string(after) {
		t.Fatal("regeneration changed analysis or manual choices")
	}
}
