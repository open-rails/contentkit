package versions

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func reviewFixture(name string, from, to int, hash string) media.File {
	f := media.File{Path: "originals/" + name + ".png", Type: "image/png", Meta: map[string]any{digestMeta: hash}}
	writePageMeta(&f, pageMetadata{SchemaVersion: 1, Name: name, SourceName: name + ".png", Span: &PageSpan{From: from, To: to}})
	return f
}

func TestPageReviewSelectionAndOverrides(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	files := []media.File{reviewFixture("001", 1, 1, "a"), reviewFixture("002-003", 2, 3, "b"), reviewFixture("004", 2, 2, "c"), reviewFixture("005", 3, 3, "d"), reviewFixture("006", 6, 6, "e"), reviewFixture("007", 7, 7, "a")}
	blank := pageMeta(files[4])
	blank.Analysis = &media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: "e", Status: "complete", Blank: "blank", Confidence: "exact"}
	writePageMeta(&files[4], blank)
	m := &media.Manifest{Files: files}
	r := reviewOf(k, m)
	if r.Selected != 2 || r.Pages[2].Decision.Reason != "covered_by_joined_page" || r.Pages[4].Decision.Reason != "blank" || r.Pages[5].Decision.Reason != "duplicate" {
		t.Fatalf("selection: %+v", r)
	}
	meta := pageMeta(m.Files[1])
	meta.Mode = "exclude"
	writePageMeta(&m.Files[1], meta)
	r = reviewOf(k, m)
	if !r.Pages[2].Included || !r.Pages[3].Included || r.Pages[1].Included {
		t.Fatalf("excluded join did not restore singles: %+v", r)
	}
	meta = pageMeta(m.Files[5])
	meta.Mode = "include"
	writePageMeta(&m.Files[5], meta)
	if !reviewOf(k, m).Pages[5].Included {
		t.Fatal("manual include lost to duplicate decision")
	}
}

func TestPageReviewOverlappingJoinsAndStableNames(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	m := &media.Manifest{Files: []media.File{reviewFixture("002-004", 2, 4, "a"), reviewFixture("003-005", 3, 5, "b")}}
	r := reviewOf(k, m)
	if r.Selected != 2 || !slices.Contains(r.Pages[0].Flags, "overlapping_joined_ranges") || !slices.Contains(r.Pages[1].Flags, "overlapping_joined_ranges") {
		t.Fatalf("overlap: %+v", r)
	}
	m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
	if reordered := reviewOf(k, m); reordered.Pages[0].ID != "003-005" || reordered.Revision == r.Revision {
		t.Fatalf("reorder changed IDs or lost revision: %+v", reordered)
	}
}

func TestPageReviewLegacyAndUnavailableAnalysis(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	f := media.File{Path: "originals/old-page-name.png", Meta: map[string]any{digestMeta: "abc"}}
	f.Meta, _ = media.WithPageAnalysis(f.Meta, media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: "abc", Status: "unsupported", Blank: "unknown", Confidence: "unknown"})
	r := reviewOf(k, &media.Manifest{Files: []media.File{f}})
	if r.Pages[0].ID != "old-page-name" || !r.Pages[0].Included || !slices.Contains(r.Pages[0].Flags, "analysis_unsupported") {
		t.Fatalf("legacy key: %+v", r)
	}
}

func TestArchivePageNamePlanPreservesGapsAndSources(t *testing.T) {
	taken := map[string]bool{}
	next := 11
	for _, test := range []struct{ source, want string }{{"1.png", "001"}, {"2.png", "002"}, {"10.png", "010"}, {"11.jpg", "011"}, {"credits.png", "012"}, {"001.webp", "013"}} {
		got, _, _ := plannedPageName(test.source, taken, &next)
		if got != test.want {
			t.Fatalf("%s -> %s, want %s", test.source, got, test.want)
		}
	}
}

func TestPageReviewRetainsOneBlankForNonblockingPublication(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	f := reviewFixture("001", 1, 1, strings.Repeat("a", 64))
	meta := pageMeta(f)
	meta.Analysis = &media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: strings.Repeat("a", 64), Status: "complete", Blank: "blank", Confidence: "exact"}
	writePageMeta(&f, meta)
	r := reviewOf(k, &media.Manifest{Files: []media.File{f}})
	if r.Selected != 1 || !slices.Contains(r.Pages[0].Flags, "review_empty_selection") {
		t.Fatalf("blank-only fallback: %+v", r)
	}
}

func TestPageReviewSegmentGuardKeepsPriorSelection(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	a, b := reviewFixture("001", 1, 1, "a"), reviewFixture("002", 2, 2, "a")
	meta := pageMeta(b)
	no := false
	meta.Included = &no
	writePageMeta(&b, meta)
	m := &media.Manifest{Files: []media.File{a, b}}
	guardSegmentedPages(k, m, true)
	r := reviewOf(k, m)
	if r.Selected != 1 || !r.Pages[0].Included || r.Pages[1].Included || !slices.Contains(r.Pages[1].Flags, "segmented_selection_preserved") {
		t.Fatalf("guard changed old inclusion: %+v", r)
	}
	guardSegmentedPages(k, m, false)
	for _, f := range m.Files {
		if pageMeta(f).SelectionGuard != nil {
			t.Fatal("removed segments retained guard")
		}
	}
}
