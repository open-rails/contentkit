package layout_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func TestParse(t *testing.T) {
	sum := sha256.Sum256(nil)
	blob := layout.SHA256Prefix + hex.EncodeToString(sum[:])
	for key, want := range map[string]layout.Key{
		"d/gallery/1/manifest.json":       {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest},
		"d/gallery/1/manifests/v2.json":   {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest, Name: "v2"},
		"d/gallery/1/blobs/" + blob:       {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaBlobs, Name: blob},
		"d/gallery/1/originals/cover":     {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaOriginals, Name: "cover"},
		"o/user/42/public/avatar_80.webp": {Tenant: "o", Kind: "user", ID: "42", Area: layout.AreaPublic, Name: "avatar_80"},
	} {
		got, ok := layout.Parse(key)
		if !ok || got != want {
			t.Errorf("%s: %+v %v", key, got, ok)
		}
	}
	for _, bad := range []string{"d/gallery/1", "d/gallery/1/blobs/x", "d/gallery/1/blobs/" + blob + "/x", "d/gallery/1/public/a.png",
		"d/../1/manifest.json", "d/gallery/1/other/x"} {
		if _, ok := layout.Parse(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}
