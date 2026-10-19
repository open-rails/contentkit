package media

import (
	"strings"
	"testing"
)

func TestSourcePageIdentityUsesOriginalHash(t *testing.T) {
	k := &Kind{Publish: &Publish{Pages: "originals/{name}"}}
	hash := strings.Repeat("a", 64)
	f := File{Path: "originals/001.png", Blob: "sha256-" + hash, Type: "image/png"}
	if k.PageID(f) != hash {
		t.Fatal("source page hash missing")
	}
	f.Path = "originals/renamed.png"
	f.Meta = map[string]any{"page": map[string]any{"name": "display-name"}}
	f.Edit = &Edit{Rotate: 90}
	if k.PageID(f) != hash {
		t.Fatal("rename/metadata/edit changed original identity")
	}
	f.Blob = "sha256-" + strings.Repeat("b", 64)
	if k.PageID(f) == hash {
		t.Fatal("replacing original bytes kept old identity")
	}
	f.Preset, f.From, f.Path = "low", "originals/renamed.png", "low/renamed.webp"
	if k.PageID(f) != "" {
		t.Fatal("a rendition was offered as a chapter source")
	}
}
