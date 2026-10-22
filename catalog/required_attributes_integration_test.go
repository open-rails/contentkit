package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/kinds"
)

func TestRequiredContentAttributesIntegration(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		gallery := o.Kinds.Kinds["gallery"]
		format := gallery.Attributes["format"]
		format.Required = true
		gallery.Attributes["format"] = format
		o.Kinds.Kinds["gallery"] = gallery
	})
	// Upload-first allocation and unrelated metadata changes remain possible.
	id := f.create("staff", gallery("Required format", "en"))
	comments := "closed"
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Comments: &comments}}}); err != nil {
		t.Fatalf("unrelated draft patch: %v", err)
	}
	if err := f.s.Publish(f.ctx, f.as("staff"), []Publish{{ContentID: id}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "format is required") {
		t.Fatalf("publish missing format: %v", err)
	}
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Attributes: map[string]any{"format": "manga"}}}}); err != nil {
		t.Fatal(err)
	}
	// Required parent attributes do not become mandatory version overrides.
	version := f.read("staff", "main.test", id)[0].Versions[0]
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: version.ID, VersionInput: VersionInput{Attributes: map[string]any{"volume": 2}}}}); err != nil {
		t.Fatalf("version inherits parent format: %v", err)
	}
	f.publish("staff", id, nil)
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Attributes: map[string]any{}}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("clearing required format: %v", err)
	}
	// Configuration is opt-in: merely declaring an enum is still optional.
	if err := kinds.ValidateRequiredAttributes(map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"manga"}}}, nil); err != nil {
		t.Fatal(err)
	}
}
