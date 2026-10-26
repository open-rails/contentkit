package critical_test

import (
	"strings"
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/importer"
)

func TestImportChaptersRequiresManifestLockingWriter(t *testing.T) {
	w := newWorld(t)
	admin := w.apps[0].engine.Client()
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Original title"}}}}})
	must(t, err)
	for _, input := range []struct {
		name     string
		chapters []catalog.Segment
		message  string
	}{
		{"numeric", []catalog.Segment{{Start: 0, End: 20}}, "numeric ranges are migration-only"},
		{"marker", []catalog.Segment{{StartPage: "pages/020.jpg"}}, "then update chapters with PUT"},
	} {
		t.Run(input.name, func(t *testing.T) {
			result, err := admin.PostImport(t.Context(), importer.Batch{Source: "chapter-lock-contract", Contents: []importer.ContentRecord{importer.ContentRecord{
				ExternalID: "content", ID: item.ID, Kind: "gallery", State: "draft",

				Versions: []importer.VersionRecord{{ExternalID: "edition", ID: item.Versions[0].ID, Languages: []string{"en"}, Segments: input.chapters, Text: map[string]catalog.VersionText{"en": {Title: "Must roll back"}}}},
			}}})
			must(t, err)
			if result.Written[importer.SectionContents] != 0 || len(result.Failed) != 1 || !strings.Contains(result.Failed[0].Error, input.message) {
				t.Fatalf("unsafe chapter import did not return an actionable refusal: %+v", result)
			}
			got, err := admin.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			must(t, err)
			if got.Title != "Original title" || len(got.Versions[0].Segments) != 0 {
				t.Fatalf("refused chapter import changed catalog state: %+v", got)
			}
		})
	}
}
