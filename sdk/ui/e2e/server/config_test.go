package main

import (
	"os"
	"testing"

	"github.com/open-rails/contentkit/media"
)

// The registry the worker reads (kinds.json) and the engine's view of it
// over the content model agree.
func TestRegistryFitsTheContentModel(t *testing.T) {
	b, err := os.ReadFile("kinds.json")
	if err != nil {
		t.Fatal(err)
	}
	mc, err := media.ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := site().Validate(); err != nil {
		t.Fatal(err)
	}
	engine, err := media.ForContent(mc, site())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []media.Config{mc, engine} {
		if _, err := media.NewRegistry(c); err != nil {
			t.Fatal(err)
		}
	}
	for i, k := range mc.Kinds {
		if k.Versions != engine.Kinds[i].Versions {
			t.Errorf("kind %s: kinds.json says versions %v, the content model %v", k.Name, k.Versions, engine.Kinds[i].Versions)
		}
	}
}
