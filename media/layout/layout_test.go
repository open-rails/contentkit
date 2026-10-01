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
