package layout_test

import (
	"crypto/sha256"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func TestParse(t *testing.T) {
	sum := sha256.Sum256(nil)
	hash := layout.SHA256Name(sum[:])
	for key, want := range map[string]layout.Key{
		"d/gallery/1/manifest.json":         {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest},
		"d/gallery/1/private/" + hash:       {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaPrivate, Name: hash},
		"accounts/user/42/public/a-64.webp": {Namespace: "accounts", Kind: "user", ID: "42", Area: layout.AreaPublic, Name: "a-64.webp"},
		"d/gallery/_default/public/c.webp":  {Namespace: "d", Kind: "gallery", ID: "_default", Area: layout.AreaPublic, Name: "c.webp"},
		"d/gallery/1/temp/u-1":              {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaTemp, Name: "u-1"},
	} {
		got, ok := layout.Parse(key)
		if !ok || got != want {
			t.Errorf("%s: %+v %v", key, got, ok)
		}
	}
	for _, bad := range []string{"d/gallery/1", "d/gallery/1/private/x", "d/gallery/1/private/" + hash + "/x",
		"d/gallery/1/originals/" + hash, "d/../1/manifest.json", "d/gallery/1/other/x", "d/gallery/1/public/.x",
		"d/gallery/1/public/a b", "d/gallery/1/private/SHA256-x"} {
		if _, ok := layout.Parse(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
	if _, ok := layout.ParseSHA256Name(hash[:len(hash)-1] + "A"); ok {
		t.Error("upper-case hex accepted")
	}
}

func TestValidStagedName(t *testing.T) {
	if !layout.ValidStagedName("u-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b") {
		t.Error("a staged name refused")
	}
	for _, bad := range []string{"u-1", "u-0192A3B4-C5D6-7E8F-9A0B-1C2D3E4F5A6B", "i-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b",
		"u-0192a3b4c5d67e8f9a0b1c2d3e4f5a6b", "u-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6g", "sha256-00"} {
		if layout.ValidStagedName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
