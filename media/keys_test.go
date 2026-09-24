package media_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// cid is the n-th test content id, a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func registry(t testing.TB) *media.Registry {
	t.Helper()
	r, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png", "image/jpeg"}, MaxBytes: 10 << 20,
			Specs: map[string]media.Spec{"thumb": {Width: 460, Height: 650, Fit: media.FitCover, Quality: 80}},
			Slots: map[string]media.Slot{"cover": {Aspect: media.Aspect3x1, Widths: []int{1500}}}},
		media.Kind{Name: "post"},
		media.Kind{Name: "user", Slots: map[string]media.Slot{"avatar": {Aspect: media.Aspect1x1, Widths: []int{320, 80}}}},
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

	gid := cid(123)
	g, err := r.Item(contentref.NewVersion("d", "gallery", gid, "0190c3"))
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := g.ManifestKey()
	bk, _ := g.Blob(blob)
	ok, _ := g.Original(blob)
	cover, _ := g.SlotOriginal("cover")
	pub, _ := g.Public("cover")
	for got, want := range map[string]string{
		mk: "d/gallery/" + gid + "/manifests/0190c3.json", bk: "d/gallery/" + gid + "/blobs/" + blob, ok: "d/gallery/" + gid + "/originals/" + blob,
		cover: "d/gallery/" + gid + "/originals/cover", pub: "d/gallery/" + gid + "/public/cover.webp", g.BlobsPrefix(): "d/gallery/" + gid + "/blobs/",
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	work, err := r.Item(contentref.New("d", "gallery", gid))
	if err != nil || work.BlobsPrefix() != g.BlobsPrefix() {
		t.Fatalf("versions share the work folder: %v", err)
	}
	if _, err := work.ManifestKey(); err == nil {
		t.Fatal("versioned kind without version has no manifest")
	}

	p, _ := r.Item(contentref.New("o", "post", cid(501)))
	if k, _ := p.ManifestKey(); k != "o/post/"+cid(501)+"/manifest.json" {
		t.Fatal(k)
	}
	u, _ := r.Item(contentref.New("o", "user", cid(42)))
	if k, _ := u.Public("avatar_80"); k != "o/user/"+cid(42)+"/public/avatar_80.webp" {
		t.Fatal(k)
	}
	if k, _ := u.SlotOriginal("avatar"); k != "o/user/"+cid(42)+"/originals/avatar" {
		t.Fatal(k)
	}
	up := media.NewUploadName()
	if k, err := p.Original(up); err != nil || k != "o/post/"+cid(501)+"/staging/"+up {
		t.Fatal(k, err)
	}

	for _, bad := range []contentref.ContentRef{
		contentref.New("d", "nope", cid(1)),
		contentref.New("d/x", "post", cid(1)),
		contentref.New("d", "post", "../1"),
		contentref.New("d", "post", ".hidden"),
		contentref.New("d", "post", cid(1)).WithVersion("v1"),
		contentref.New("d", "post", "é"),
		contentref.New("d", "post", "1"),
		contentref.New("d", "post", strings.ToUpper(cid(1))),
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
	for _, bad := range []media.Slot{{Aspect: media.Aspect1x1}, {Aspect: media.Aspect{W: -1, H: 1}, Widths: []int{64}}, {Aspect: media.Aspect1x1, Widths: []int{64, 64}}, {Aspect: media.Aspect1x1, Widths: []int{0}}} {
		if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{"cover": bad}}); err == nil {
			t.Fatalf("slot %+v accepted", bad)
		}
	}
	if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{"a.json": {Aspect: media.Aspect1x1, Widths: []int{8}}}}); err == nil {
		t.Fatal("slot name colliding with a record accepted")
	}
	u, _ := r.Kind("user")
	if w := u.Slots["avatar"].Widths; w[0] != 80 || w[1] != 320 {
		t.Fatalf("widths not sorted: %v", w)
	}
	inline := media.NewInlineName()
	if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{inline: {Aspect: media.Aspect1x1, Widths: []int{8}}}}); err == nil {
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
	pid := cid(1)
	p, _ := r.Item(contentref.New("h", "post", pid))
	name := media.NewInlineName()
	orig, err := p.SlotOriginal(name)
	if err != nil || orig != "h/post/"+pid+"/originals/"+name {
		t.Fatal(orig, err)
	}
	if !p.Inline(name) || p.Inline("cover") {
		t.Fatal("inline names")
	}
	if _, err := p.SlotRecord(name); err == nil {
		t.Fatal("inline image has a slot record")
	}
	if k, _ := p.Public(name); k != "h/post/"+pid+"/public/"+name+".webp" {
		t.Fatal(k)
	}
	for _, bad := range []string{"i-1", "i-" + strings.ToUpper(name[2:]), "cover"} {
		if _, err := p.SlotOriginal(bad); err == nil {
			t.Errorf("inline name %q accepted", bad)
		}
	}
	g, _ := r.Item(contentref.New("h", "gallery", cid(2)))
	if _, err := g.SlotOriginal(name); err == nil {
		t.Fatal("inline image accepted by a kind without Inline")
	}
}

func TestSlotResolve(t *testing.T) {
	cover := media.Slot{Aspect: media.Aspect3x1, Widths: []int{300, 600}}
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
		Slots: map[string]media.Slot{"banner": {Aspect: media.Aspect3x1, Widths: []int{600}}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := r.Kind("video")
	if p, ok := k.Slots[media.PosterSlot]; !ok || p.Hash() != media.VideoPoster.Hash() || len(k.Slots) != 2 {
		t.Fatalf("slots %+v", k.Slots)
	}
	vid := cid(9)
	v, _ := r.Item(contentref.New("h", "video", vid))
	// Posters and hover previews render to editor/ and are published to public/.
	if v.HoverPreviewRecord() != "h/video/"+vid+"/originals/hover_preview.json" || v.HoverPreviewOutput(640, true) != "h/video/"+vid+"/editor/hover_preview_640.mp4" ||
		v.HoverPreviewPublic(640, true) != "h/video/"+vid+"/public/hover_preview_640.mp4" {
		t.Fatal(v.HoverPreviewRecord(), v.HoverPreviewOutput(640, true))
	}
	if k, _ := v.SlotOutput(media.PosterSlot, 960); k != "h/video/"+vid+"/editor/poster_960.webp" {
		t.Fatal(k)
	}
	if k, _ := v.SlotPublic(media.PosterSlot, 960); k != "h/video/"+vid+"/public/poster_960.webp" {
		t.Fatal(k)
	}
	if k, _ := v.SlotOutput("banner", 600); k != "h/video/"+vid+"/public/banner_600.webp" {
		t.Fatal("ungated slot", k)
	}
	for _, name := range []string{media.PosterSlot, media.HoverPreview, "exposure"} {
		if _, err := media.NewRegistry(media.Kind{Name: "video", Video: &media.Video{},
			Slots: map[string]media.Slot{name: {Aspect: media.Aspect16x9, Widths: []int{320}}}}); err == nil {
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

func TestSlotRungsCapAtTheEditedWidth(t *testing.T) {
	s := media.Slot{Aspect: media.Aspect3x1, Widths: []int{900, 3000}, MinWidth: 600}
	if s.Min() != 600 {
		t.Fatalf("min %d", s.Min())
	}
	for edited, want := range map[int][]int{600: {600, 600}, 900: {900, 900}, 1200: {900, 1200}, 5000: {900, 3000}} {
		got := []int{s.OutputWidth(900, edited), s.OutputWidth(3000, edited)}
		if !slices.Equal(got, want) {
			t.Errorf("edited %d: %v, want %v", edited, got, want)
		}
	}
	if _, err := s.Resolve(&media.Edit{Crop: &media.Crop{W: 550, H: 183}}, 1800, 1200); err == nil {
		t.Fatal("crop under MinWidth accepted")
	}
}

func TestAspect(t *testing.T) {
	for in, want := range map[string]media.Aspect{"3:1": media.Aspect3x1, "6:2": media.Aspect3x1, "21:9": media.Aspect21x9, "9:16": media.Aspect9x16, "1080:1920": media.Aspect9x16, "": media.AspectNative, "native": media.AspectNative} {
		if got, err := media.ParseAspect(in); err != nil || got != want {
			t.Errorf("ParseAspect(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"3", "3:", ":1", "0:1", "-3:1", "3.5:1", "a:b", "10001:1", "1:2:3"} {
		if _, err := media.ParseAspect(bad); err == nil {
			t.Errorf("ParseAspect(%q) accepted", bad)
		}
	}
	if media.Ratio("21:9").String() != "7:3" || media.Aspect16x9.String() != "16:9" || media.AspectNative.String() != "native" {
		t.Fatal("String")
	}
	// Heights at the widths hosts use, rounded half up.
	for _, c := range []struct {
		a    media.Aspect
		w, h int
	}{{media.Aspect3x1, 900, 300}, {media.Aspect3x1, 1100, 367}, {media.Aspect3x1, 3000, 1000}, {media.Aspect16x9, 640, 360}, {media.Aspect16x9, 1280, 720},
		{media.Aspect16x9, 1098, 618}, {media.Aspect9x16, 640, 1138}, {media.Aspect1x1, 128, 128}, {media.Aspect4x5, 1080, 1350}, {media.Aspect21x9, 2560, 1097}} {
		if got := c.a.Height(c.w); got != c.h {
			t.Errorf("%v height of %d: %d, want %d", c.a, c.w, got, c.h)
		}
	}
	if media.Aspect3x1.Width(300) != 900 || media.AspectNative.Height(100) != 0 || media.AspectOf(1920, 1080) != media.Aspect16x9 {
		t.Fatal("Width/native/AspectOf")
	}
	type doc struct {
		A media.Aspect `json:"a"`
		N media.Aspect `json:"n"`
	}
	b, err := json.Marshal(doc{A: media.Aspect3x1})
	if err != nil || string(b) != `{"a":"3:1","n":""}` {
		t.Fatalf("marshal %s %v", b, err)
	}
	var back doc
	if err := json.Unmarshal([]byte(`{"a":"6:2","n":""}`), &back); err != nil || back.A != media.Aspect3x1 || !back.N.Native() {
		t.Fatalf("unmarshal %+v %v", back, err)
	}
	if json.Unmarshal([]byte(`{"a":3}`), &back) == nil || json.Unmarshal([]byte(`{"a":"0:1"}`), &back) == nil {
		t.Fatal("invalid aspect JSON accepted")
	}
	if (media.Slot{Aspect: media.Aspect{W: 6, H: 2}, Widths: []int{1}}).Aspect.Valid() {
		t.Fatal("unreduced aspect valid")
	}
}
