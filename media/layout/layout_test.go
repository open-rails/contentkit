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
	const staged = "u-0190f3b2-7c1e-7a3d-9e4f-0123456789ab"
	for key, want := range map[string]layout.Key{
		"d/gallery/1/manifest.json":              {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest},
		"d/gallery/1/manifests/v2.json":          {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest, Name: "v2"},
		"d/gallery/1/blobs/" + blob:              {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaBlobs, Name: blob},
		"d/gallery/1/originals/cover":            {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaOriginals, Name: "cover"},
		"o/user/42/public/avatar_80.webp":        {Tenant: "o", Kind: "user", ID: "42", Area: layout.AreaPublic, Name: "avatar_80"},
		"h/video/9/public/hover_preview_320.mp4": {Tenant: "h", Kind: "video", ID: "9", Area: layout.AreaPublic, Name: "hover_preview_320"},
		"h/video/9/editor/hover_preview_320.mp4": {Tenant: "h", Kind: "video", ID: "9", Area: layout.AreaEditor, Name: "hover_preview_320"},
		"h/video/9/editor/poster_480.webp":       {Tenant: "h", Kind: "video", ID: "9", Area: layout.AreaEditor, Name: "poster_480"},
		"d/gallery/1/editor/" + blob:             {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaEditor, Name: blob},
		"d/gallery/1/staging/" + staged:          {Tenant: "d", Kind: "gallery", ID: "1", Area: layout.AreaStaging, Name: staged},
	} {
		got, ok := layout.Parse(key)
		if !ok || got != want {
			t.Errorf("%s: %+v %v", key, got, ok)
		}
	}
	for _, bad := range []string{"d/gallery/1", "d/gallery/1/blobs/x", "d/gallery/1/blobs/" + blob + "/x", "d/gallery/1/public/a.png", "d/gallery/1/editor/x", "d/gallery/1/editor/a.png",
		"d/../1/manifest.json", "d/gallery/1/other/x", "d/gallery/1/staging/" + blob, "d/gallery/1/staging/cover"} {
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
		if got := layout.ValidBlobName(name); got != want {
			t.Errorf("ValidBlobName(%q) = %v", name, got)
		}
	}
	if !layout.ValidInlineName("i-"+id) || layout.ValidInlineName("u-"+id) {
		t.Error("ValidInlineName")
	}
	if !layout.ValidStagedName("u-"+id) || layout.ValidStagedName("i-"+id) {
		t.Error("ValidStagedName")
	}
	if layout.SourceArea("u-"+id) != layout.AreaStaging || layout.SourceArea(layout.SHA256Prefix+id) != layout.AreaOriginals {
		t.Error("SourceArea")
	}
}
