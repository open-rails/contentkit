package media_test

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

func registry(t testing.TB) *media.Registry {
	t.Helper()
	r, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png", "image/jpeg"}, MaxBytes: 10 << 20,
			Specs: map[string]media.Spec{"thumb": {Width: 460, Height: 650, Fit: media.FitCover, Quality: 80}},
			Slots: map[string]media.Slot{"cover": {Aspect: 3, Widths: []int{1500}}}},
		media.Kind{Name: "post"},
		media.Kind{Name: "user", Slots: map[string]media.Slot{"avatar": {Aspect: 1, Widths: []int{320, 80}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestItemKeys(t *testing.T) {
	r := registry(t)
	sum := sha256.Sum256([]byte("page"))
	blob := media.SHA256Name(sum[:])

	g, err := r.Item(contentref.NewVersion("d", "gallery", "123", "0190c3"))
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := g.ManifestKey()
	bk, _ := g.Blob(blob)
	ok, _ := g.Original(blob)
	cover, _ := g.SlotOriginal("cover")
	pub, _ := g.Public("cover")
	for got, want := range map[string]string{
		mk: "d/gallery/123/manifests/0190c3.json", bk: "d/gallery/123/blobs/" + blob, ok: "d/gallery/123/originals/" + blob,
		cover: "d/gallery/123/originals/cover", pub: "d/gallery/123/public/cover.webp", g.BlobsPrefix(): "d/gallery/123/blobs/",
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	work, err := r.Item(contentref.New("d", "gallery", "123"))
	if err != nil || work.BlobsPrefix() != g.BlobsPrefix() {
		t.Fatalf("versions share the work folder: %v", err)
	}
	if _, err := work.ManifestKey(); err == nil {
		t.Fatal("versioned kind without version has no manifest")
	}

	p, _ := r.Item(contentref.New("o", "post", "501"))
	if k, _ := p.ManifestKey(); k != "o/post/501/manifest.json" {
		t.Fatal(k)
	}
	u, _ := r.Item(contentref.New("o", "user", "42"))
	if k, _ := u.Public("avatar_80"); k != "o/user/42/public/avatar_80.webp" {
		t.Fatal(k)
	}
	if k, _ := u.SlotOriginal("avatar"); k != "o/user/42/originals/avatar" {
		t.Fatal(k)
	}
	up := media.NewUploadName()
	if k, err := p.Original(up); err != nil || k != "o/post/501/originals/"+up {
		t.Fatal(k, err)
	}

	for _, bad := range []contentref.ContentRef{
		contentref.New("d", "nope", "1"),
		contentref.New("d/x", "post", "1"),
		contentref.New("d", "post", "../1"),
		contentref.New("d", "post", ".hidden"),
		contentref.New("d", "post", "1").WithVersion("v1"),
		contentref.New("d", "post", "é"),
	} {
		if _, err := r.Item(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, bad := range []string{"", "sha256-XYZ", "sha256-" + blob[7:20], "u-not-a-uuid", "../x", "cover"} {
		if _, err := p.Blob(bad); err == nil {
			t.Errorf("blob name %q accepted", bad)
		}
	}
	if _, err := p.SlotOriginal("cover"); err == nil {
		t.Error("unregistered slot accepted")
	}
	if _, err := p.Public(blob); err == nil {
		t.Error("blob-like public name accepted")
	}
}

func TestKindRules(t *testing.T) {
	r := registry(t)
	g, _ := r.Kind("gallery")
	if err := g.Allows("image/png", 10<<20); err != nil {
		t.Fatal(err)
	}
	if err := g.Allows("image/gif", 1); !errors.Is(err, media.ErrType) {
		t.Fatal(err)
	}
	if err := g.Allows("image/png", 10<<20+1); !errors.Is(err, media.ErrTooLarge) {
		t.Fatal(err)
	}
	if s := g.Specs["thumb"]; s.Hash() == (media.Spec{Width: 460, Height: 650, Fit: media.FitCover, Quality: 90}).Hash() || s.Hash() != s.Hash() {
		t.Fatal("spec hash must track every field and be stable")
	}
	if _, err := media.NewRegistry(media.Kind{Name: "a/b"}); err == nil {
		t.Fatal("invalid kind name accepted")
	}
	if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{"cover": {}}}); err == nil {
		t.Fatal("empty slot accepted")
	}
	for _, bad := range []media.Slot{{Aspect: 1}, {Aspect: -1, Widths: []int{64}}, {Aspect: 1, Widths: []int{64, 64}}, {Aspect: 1, Widths: []int{0}}} {
		if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{"cover": bad}}); err == nil {
			t.Fatalf("slot %+v accepted", bad)
		}
	}
	if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{"a.json": {Aspect: 1, Widths: []int{8}}}}); err == nil {
		t.Fatal("slot name colliding with a record accepted")
	}
	u, _ := r.Kind("user")
	if w := u.Slots["avatar"].Widths; w[0] != 80 || w[1] != 320 {
		t.Fatalf("widths not sorted: %v", w)
	}
	inline := media.NewInlineName()
	if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{inline: {Aspect: 1, Widths: []int{8}}}}); err == nil {
		t.Fatal("inline-named slot accepted")
	}
	editor := media.Spec{Unedited: true, EditorOnly: true}
	for name, k := range map[string]media.Kind{
		"public unedited spec": {Specs: map[string]media.Spec{"editor": {Unedited: true}}},
		"editor inline":        {Inline: &editor},
		"editor zip":           {Specs: map[string]media.Spec{"editor": editor}, Zip: "editor"},
		"odd ladder":           {Video: &media.Video{Ladder: []int{720, 481}}},
		"ascending ladder":     {Video: &media.Video{Ladder: []int{480, 720}}},
		"wide min aspect":      {Video: &media.Video{MinAspect: 1.2}},
		"narrow max aspect":    {Video: &media.Video{MaxAspect: 0.8}},
		"negative aspect":      {Video: &media.Video{MinAspect: -1}},
	} {
		k.Name = "x"
		if _, err := media.NewRegistry(k); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := media.NewRegistry(media.Kind{Name: "x", Specs: map[string]media.Spec{"editor": editor}, Video: &media.Video{Ladder: []int{720, 360}}}); err != nil {
		t.Fatal(err)
	}
}

func TestInlineImageKeys(t *testing.T) {
	spec := media.Spec{Width: 1600, Fit: media.FitInside, Quality: 85}
	r, err := media.NewRegistry(media.Kind{Name: "post", Inline: &spec}, media.Kind{Name: "gallery"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.Item(contentref.New("h", "post", "p1"))
	name := media.NewInlineName()
	orig, err := p.SlotOriginal(name)
	if err != nil || orig != "h/post/p1/originals/"+name {
		t.Fatal(orig, err)
	}
	if !p.Inline(name) || p.Inline("cover") {
		t.Fatal("inline names")
	}
	if _, err := p.SlotRecord(name); err == nil {
		t.Fatal("inline image has a slot record")
	}
	if k, _ := p.Public(name); k != "h/post/p1/public/"+name+".webp" {
		t.Fatal(k)
	}
	for _, bad := range []string{"i-1", "i-" + strings.ToUpper(name[2:]), "cover"} {
		if _, err := p.SlotOriginal(bad); err == nil {
			t.Errorf("inline name %q accepted", bad)
		}
	}
	g, _ := r.Item(contentref.New("h", "gallery", "g1"))
	if _, err := g.SlotOriginal(name); err == nil {
		t.Fatal("inline image accepted by a kind without Inline")
	}
}

func TestSlotResolve(t *testing.T) {
	cover := media.Slot{Aspect: 3, Widths: []int{300, 600}}
	crop := func(x, y, w, h int) *media.Edit { return &media.Edit{Crop: &media.Crop{X: x, Y: y, W: w, H: h}} }
	for _, c := range []struct {
		edit *media.Edit
		w, h int
		want *media.Edit
	}{
		{nil, 900, 900, crop(0, 300, 900, 300)},                          // centred in a square
		{nil, 3000, 600, crop(600, 0, 1800, 600)},                        // centred in a wider image
		{crop(100, 200, 600, 0), 1000, 1000, crop(100, 200, 600, 200)},   // height follows width
		{crop(100, 200, 600, 999), 1000, 1000, crop(100, 200, 600, 200)}, // whatever height is sent
		{&media.Edit{Rotate: 90}, 900, 900, &media.Edit{Crop: &media.Crop{X: 300, Y: 0, W: 300, H: 900}, Rotate: 90}},
		{crop(500, 0, 600, 0), 1000, 1000, nil}, // outside
		{crop(0, 0, 200, 0), 1000, 1000, nil},   // under the smallest width
	} {
		got, err := cover.Resolve(c.edit, c.w, c.h)
		if c.want == nil {
			if err == nil {
				t.Errorf("Resolve(%+v, %d, %d) accepted %+v", c.edit, c.w, c.h, got)
			}
			continue
		}
		if err != nil || got.Hash() != c.want.Hash() {
			t.Errorf("Resolve(%+v, %d, %d) = %+v %+v, %v", c.edit, c.w, c.h, got, got.Crop, err)
		}
	}
}

func TestVideoKindSlots(t *testing.T) {
	r, err := media.NewRegistry(media.Kind{Name: "video", Versioned: true, Video: &media.Video{},
		Slots: map[string]media.Slot{"banner": {Aspect: 3, Widths: []int{600}}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := r.Kind("video")
	if p, ok := k.Slots[media.PosterSlot]; !ok || p.Hash() != media.VideoPoster.Hash() || len(k.Slots) != 2 {
		t.Fatalf("slots %+v", k.Slots)
	}
	v, _ := r.Item(contentref.New("h", "video", "9"))
	// Posters and hover previews render to editor/ and are published to public/.
	if v.HoverPreviewRecord() != "h/video/9/originals/hover_preview.json" || v.HoverPreviewOutput(640, true) != "h/video/9/editor/hover_preview_640.mp4" ||
		v.HoverPreviewPublic(640, true) != "h/video/9/public/hover_preview_640.mp4" {
		t.Fatal(v.HoverPreviewRecord(), v.HoverPreviewOutput(640, true))
	}
	if k, _ := v.SlotOutput(media.PosterSlot, 960); k != "h/video/9/editor/poster_960.webp" {
		t.Fatal(k)
	}
	if k, _ := v.SlotPublic(media.PosterSlot, 960); k != "h/video/9/public/poster_960.webp" {
		t.Fatal(k)
	}
	if k, _ := v.SlotOutput("banner", 600); k != "h/video/9/public/banner_600.webp" {
		t.Fatal("ungated slot", k)
	}
	for _, name := range []string{media.PosterSlot, media.HoverPreview, "exposure"} {
		if _, err := media.NewRegistry(media.Kind{Name: "video", Video: &media.Video{},
			Slots: map[string]media.Slot{name: {Aspect: 16.0 / 9, Widths: []int{320}}}}); err == nil {
			t.Errorf("reserved slot %q accepted", name)
		}
	}
	if s := media.HoverPreviewSizes(270, 480); len(s) != 1 || s[0] != (media.Dims{W: 320, H: 180}) {
		t.Fatalf("portrait sizes %v", s)
	}
	if s := media.HoverPreviewSizes(1920, 1080); len(s) != 2 {
		t.Fatalf("1080p sizes %v", s)
	}
	for in, want := range map[media.Dims]media.Dims{{W: 1920, H: 1080}: {W: 1920, H: 1080}, {W: 426, H: 240}: {W: 640, H: 361}, {W: 270, H: 480}: {W: 640, H: 1138}} {
		if got := media.PosterFrameSize(media.VideoPoster, in.W, in.H); got != want {
			t.Errorf("poster frame of %v: %v, want %v", in, got, want)
		}
	}
	for d, want := range map[float64][2]float64{12: {3, 3}, 2: {0, 2}, 3.5: {0.5, 3}, 600: {150, 3}} {
		if s, l := media.AutoHoverPreview(d); s != want[0] || l != want[1] {
			t.Errorf("auto preview of %gs: %g+%g", d, s, l)
		}
	}
}

func TestNativeSlot(t *testing.T) {
	s := media.VideoPoster
	if !s.Native() || s.Size(480, media.Dims{W: 1080, H: 1920}) != (media.Dims{W: 480, H: 853}) {
		t.Fatalf("native size %v", s.Size(480, media.Dims{W: 1080, H: 1920}))
	}
	// Without a crop the whole image; a crop keeps its own shape.
	if e, err := s.Resolve(nil, 1080, 1920); err != nil || e.Crop != nil {
		t.Fatalf("uncropped %+v %v", e, err)
	}
	if e, err := s.Resolve(&media.Edit{Crop: &media.Crop{W: 700, H: 700}}, 1080, 1920); err != nil || e.Crop.H != 700 {
		t.Fatalf("square crop %+v %v", e, err)
	}
	if _, err := s.Resolve(&media.Edit{Crop: &media.Crop{W: 300, H: 600}}, 1080, 1920); err == nil {
		t.Fatal("crop under Min accepted")
	}
	stamp := media.NewSlotStamp("v1", []media.Dims{{W: 480, H: 853}, {W: 960, H: 1707}})
	if stamp != "v1:480x853,960x1707" {
		t.Fatal(stamp)
	}
	if v, outs, err := stamp.Parse(); err != nil || v != "v1" || len(outs) != 2 || outs[1] != (media.Dims{W: 960, H: 1707}) {
		t.Fatalf("parse %q %v %v", v, outs, err)
	}
	if _, outs, err := media.SlotStamp("v1:480,960").Parse(); err != nil || outs[0] != (media.Dims{W: 480}) {
		t.Fatalf("bare widths %v %v", outs, err)
	}
}

func TestPosterWidthsPolicy(t *testing.T) {
	r, err := media.NewRegistry(media.Kind{Name: "v", Video: &media.Video{PosterWidths: []int{1440, 720}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := r.Kind("v")
	if p := k.Slots[media.PosterSlot]; !p.Native() || len(p.Widths) != 2 || p.Widths[0] != 720 || p.Min() != 720 {
		t.Fatalf("poster %+v", p)
	}
	if got := media.PosterFrameSize(k.Slots[media.PosterSlot], 640, 360); got != (media.Dims{W: 720, H: 405}) {
		t.Fatalf("frame %v", got)
	}
	if _, err := media.NewRegistry(media.Kind{Name: "v", Video: &media.Video{PosterWidths: []int{720, 720}}}); err == nil {
		t.Fatal("duplicate poster widths accepted")
	}
}
