package importer_test

import (
	"errors"
	"testing"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/importer"
)

func TestImportInternalEntityClassification(t *testing.T) {
	f := newEngine(t)
	record := importer.GroupRecord{ExternalID: "internal-marker", ID: id("tag", 91), Kind: "tag", Slug: "restricted", Internal: ptr(true),
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Restricted"}}}
	batch := importer.Batch{Source: "test-internal", Groups: []importer.GroupRecord{record}}
	result := f.importBatch(t, batch)
	if len(result.Failed) != 0 || result.Written[importer.SectionGroups] != 1 {
		t.Fatalf("import internal entity: %+v", result)
	}
	if got := f.scalar(t, "SELECT internal::text FROM $s.content_groups WHERE id=$1", record.ID); got != "true" {
		t.Fatalf("stored internal=%q", got)
	}
	_, err := f.e.Client().GetEntity(t.Context(), record.ID, contentkit.GetEntityQuery{Language: "en"})
	var responseError *contentkit.Error
	if !errors.As(err, &responseError) || responseError.Status != 404 {
		t.Fatalf("imported internal entity is public: %v", err)
	}
	batch.Groups[0].Internal = nil
	result = f.importBatch(t, batch)
	if len(result.Failed) != 0 || f.scalar(t, "SELECT internal::text FROM $s.content_groups WHERE id=$1", record.ID) != "true" {
		t.Fatalf("omitted classification changed existing internal entity: %+v", result)
	}
	batch.Groups[0].Internal = ptr(false)
	result = f.importBatch(t, batch)
	if len(result.Failed) != 0 {
		t.Fatalf("update classification: %+v", result)
	}
	if _, err := f.e.Client().GetEntity(t.Context(), record.ID, contentkit.GetEntityQuery{Language: "en"}); err != nil {
		t.Fatalf("explicitly public import remains hidden: %v", err)
	}
}
