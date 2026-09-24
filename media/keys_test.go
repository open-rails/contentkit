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
			Slots: map[string]media.Slot{"cover": {Outputs: map[string]media.Spec{"cover": {Width: 460, Quality: 80}}}}},
		media.Kind{Name: "post"},
		media.Kind{Name: "user", Slots: map[string]media.Slot{"avatar": {Outputs: map[string]media.Spec{
			"avatar_80": {Width: 80, Height: 80, Fit: media.FitCover}, "avatar_320": {Width: 320, Height: 320, Fit: media.FitCover}}}}},
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
	inline := media.NewInlineName()
	if _, err := media.NewRegistry(media.Kind{Name: "x", Slots: map[string]media.Slot{inline: {Outputs: map[string]media.Spec{"a": {}}}}}); err == nil {
		t.Fatal("inline-named slot accepted")
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
	outs, err := p.SlotOutputs(name)
	if err != nil || len(outs) != 1 || outs[name] != spec {
		t.Fatal(outs, err)
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
