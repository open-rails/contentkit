package layout_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func TestParse(t *testing.T) {
	sum := sha256.Sum256(nil)
	hash := layout.SHA256Prefix + hex.EncodeToString(sum[:])
	const staged = "u-0190f3b2-7c1e-7a3d-9e4f-0123456789ab"
	editor := layout.EditorPrefix + hex.EncodeToString(sum[:])
	for key, want := range map[string]layout.Key{
		"d/gallery/1/manifest.json":     {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest},
		"d/gallery/1/originals/" + hash: {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaOriginals, Name: hash},
		"d/gallery/1/private/" + hash:   {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaPrivate, Name: hash},
		"o/user/42/public/" + hash:      {Tenant: "o", Kind: "user", ID: "42", Area: layout.AreaPublic, Name: hash},
		"d/gallery/1/temp/" + staged:    {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaTemp, Name: staged},
		"d/gallery/1/temp/" + editor:    {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaTemp, Name: editor},
	} {
		got, ok := layout.Parse(key)
		if !ok || got != want {
			t.Errorf("%s: %+v %v", key, got, ok)
		}
	}
	for _, bad := range []string{"d/gallery/1", "d/gallery/1/private/x", "d/gallery/1/private/" + hash + "/x", "d/gallery/1/public/a.webp",
		"d/gallery/1/originals/cover", "d/gallery/1/manifests/v2.json", "d/gallery/1/blobs/" + hash, "d/gallery/1/editor/" + hash,
		"d/../1/manifest.json", "d/gallery/1/other/x", "d/gallery/1/staging/" + staged, "d/gallery/1/temp/" + hash,
		"d/gallery/1/private/" + staged, "d/gallery/1/private/" + editor, "d/gallery/1/temp/e-ABC"} {
		if _, ok := layout.Parse(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestUUIDNames(t *testing.T) {
	const id = "0190f3b2-7c1e-7a3d-9e4f-0123456789ab"
	for name, want := range map[string]bool{
		"u-" + id:                                true,
		"u-0190F3B2-7C1E-7A3D-9E4F-0123456789AB": false,
		"u-0190f3b27c1e7a3d9e4f0123456789ab":     false,
		"u-0190f3b2-7c1e-7a3d-9e4f-0123456789ag": false,
		"u-0190f3b2x7c1e-7a3d-9e4f-0123456789ab": false,
		"i-" + id:                                false,
	} {
		if got := layout.ValidSourceName(name); got != want {
			t.Errorf("ValidSourceName(%q) = %v", name, got)
		}
	}
	if !layout.ValidInlineName("i-"+id) || layout.ValidInlineName("u-"+id) {
		t.Error("ValidInlineName")
	}
	if !layout.ValidStagedName("u-"+id) || layout.ValidStagedName("i-"+id) {
		t.Error("ValidStagedName")
	}
	if layout.SourceArea("u-"+id) != layout.AreaTemp || layout.SourceArea(layout.SHA256Prefix+id) != layout.AreaOriginals {
		t.Error("SourceArea")
	}
}
