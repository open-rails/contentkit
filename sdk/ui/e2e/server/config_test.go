package main

import (
	"os"
	"testing"

	contentkit "github.com/open-rails/contentkit"
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
	input, avatars, err := splitMedia(mc)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := (contentkit.Config{Tenant: tenant, Content: site(), Avatars: avatars, Media: &contentkit.MediaConfig{Config: input}}).MediaKinds()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := media.NewRegistry(mc)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := media.NewRegistry(engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mc.Kinds {
		other, err := registry.Kind(k.Name)
		if err != nil || k.Versions != other.Versions {
			t.Errorf("kind %s: worker and engine disagree", k.Name)
		}
	}
	if worker.Config().Avatars != registry.Config().Avatars {
		t.Fatal("worker and engine disagree on avatars")
	}
}
