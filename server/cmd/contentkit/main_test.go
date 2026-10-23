package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/media"
)

const config = `
engine:
  tenant: site
  content:
    languages: [en]
    kinds:
      gallery: {route: g, presentation: reader, versions: {languages: one}, entities: [tag]}
    entity_kinds:
      tag: {route: tags}
      channel: {route: c, owner: user}
  media:
    base_url: https://media.site.test
    kinds:
      - name: gallery
        uploads: [{path: "pages/{name}", types: [image/png], max_bytes: 1048576}]
`

// media-config prints the engine's registry as the stock media worker loads
// it (MEDIA_KINDS_FILE): content kinds versioned, every entity kind with its
// cover media and a user-owned one with avatars, in the tenant's namespace.
func TestMediaConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contentkit.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"media-config", "-config", path}, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := media.ParseConfig(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Config().Namespace; got != "site" {
		t.Fatalf("namespace %q", got)
	}
	presets := func(name string) []string {
		k, err := reg.Kind(name)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range k.Public {
			out = append(out, p.Name)
		}
		return out
	}
	if k, err := reg.Kind("gallery"); err != nil || !k.Versions {
		t.Fatalf("gallery %+v %v", k, err)
	}
	if got := presets("tag"); !slices.Equal(got, []string{media.CoverPreset}) {
		t.Fatalf("tag presets %v", got)
	}
	if got := presets("channel"); !slices.Equal(got, []string{media.CoverPreset, media.AvatarPreset}) {
		t.Fatalf("channel presets %v", got)
	}
	if err := run(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"nope", "-config", path}, &out); err == nil {
		t.Fatal("an unknown command ran")
	}
}
