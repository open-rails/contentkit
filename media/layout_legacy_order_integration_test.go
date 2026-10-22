package media_test

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// Storage migration is not an edit: historical derived-file order and download
// names survive even when the current kind would canonicalize them differently.
func TestLayoutMigrationPreservesLegacyManifestOrder(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	raw, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	old := seedNestedVersion(t, f, ref)
	cfg := f.reg.Config()
	cfg.Kinds = slices.Clone(cfg.Kinds)
	for i := range cfg.Kinds {
		k := &cfg.Kinds[i]
		if k.Name != "gallery" {
			continue
		}
		k.Private = slices.DeleteFunc(slices.Clone(k.Private), func(p media.Private) bool { return p.Name == "high" })
		for n := range k.Private {
			if k.Private[n].Name == "zip" {
				k.Private[n].Download = "current-name.zip"
			}
		}
	}
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := reg.Kind("gallery")
	canonical := raw.Clone()
	kind.Normalize(canonical)
	encoded := func(m *media.Manifest) []byte {
		t.Helper()
		m = m.Clone()
		m.Receipt = nil
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if bytes.Equal(encoded(raw), encoded(canonical)) {
		t.Fatal("fixture did not retain historical order and download metadata")
	}
	manifests := s3test.Manifests(t, f.env.Store, reg, media.ManifestOptions{Journal: f.journal})
	mapping := []media.LayoutMapping{{From: ref, To: ref, SourceFolder: old}}
	plan, err := media.PlanLayoutMigration(t.Context(), manifests, reg, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded(raw), encoded(plan.Entries[0].Manifest)) {
		t.Fatal("planning normalized historical manifest data")
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store)})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := media.ApplyLayoutMigration(t.Context(), manifests, jobs, plan); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := manifests.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded(raw), encoded(got)) {
		t.Fatal("storage copy changed historical order, filenames or metadata")
	}
	for _, object := range plan.Entries[0].Objects {
		get := func(key string) []byte {
			t.Helper()
			r, _, err := f.env.Store.Get(t.Context(), key, media.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			b, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		if !bytes.Equal(get(object.Source), get(object.Target)) {
			t.Fatal("storage copy changed source bytes")
		}
	}
	// A genuinely different destination still cannot reinterpret the sources.
	other := reg.Config()
	other.Kinds = slices.Clone(other.Kinds)
	for i := range other.Kinds {
		if other.Kinds[i].Name == "gallery" {
			other.Kinds[i].Private = slices.Clone(other.Kinds[i].Private)
			for n := range other.Kinds[i].Private {
				if other.Kinds[i].Private[n].Name == "zip" {
					other.Kinds[i].Private[n].Download = "different-name.zip"
				}
			}
		}
	}
	incompatible, err := media.NewRegistry(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := media.PlanLayoutMigration(t.Context(), manifests, incompatible, mapping); err == nil || !strings.Contains(err.Error(), "changes source manifest semantics") {
		t.Fatalf("incompatible target kind accepted: %v", err)
	}
	// The copy exception must not disable canonicalization for ordinary edits.
	edited, err := manifests.Edit(t.Context(), ref, func(m *media.Manifest) error {
		m.Meta = map[string]any{"title": "edited after migration"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edited.Files, canonical.Files) {
		t.Fatal("ordinary edit did not normalize historical file metadata")
	}
}
