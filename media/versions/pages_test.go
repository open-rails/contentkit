package versions

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func reviewHash(seed string) string {
	if len(seed) == 64 {
		return seed
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func reviewFixture(name string, from, to int, hash string) media.File {
	hash = reviewHash(hash)
	f := media.File{Path: "originals/" + name + ".png", Blob: "sha256-" + hash, Type: "image/png", Meta: map[string]any{digestMeta: hash}}
	writePageMeta(&f, pageMetadata{SchemaVersion: 1, Name: name, SourceName: name + ".png", Span: &PageSpan{From: from, To: to}})
	return f
}

func TestPageSelectionAndRestore(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	files := []media.File{reviewFixture("001", 1, 1, "a"), reviewFixture("002-003", 2, 3, "b"), reviewFixture("004", 2, 2, "c"), reviewFixture("005", 3, 3, "d"), reviewFixture("006", 6, 6, "e"), reviewFixture("007", 7, 7, "a")}
	blank := pageMeta(files[4])
	blank.Analysis = &media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: reviewHash("e"), Status: "complete", Blank: "blank", Confidence: "exact"}
	writePageMeta(&files[4], blank)
	m := &media.Manifest{Files: files}
	r := selectionOf(k, m)
	if r.Selected != 2 || r.Pages[2].Decision.Reason != "covered_by_joined_page" || r.Pages[4].Decision.Reason != "blank" || r.Pages[5].Decision.Reason != "duplicate" {
		t.Fatalf("selection: %+v", r)
	}
	meta := pageMeta(m.Files[1])
	meta.Mode = "exclude"
	writePageMeta(&m.Files[1], meta)
	r = selectionOf(k, m)
	if !r.Pages[2].Included || !r.Pages[3].Included || r.Pages[1].Included {
		t.Fatalf("excluded join did not restore singles: %+v", r)
	}
	meta = pageMeta(m.Files[5])
	meta.Mode = "include"
	writePageMeta(&m.Files[5], meta)
	if !selectionOf(k, m).Pages[5].Included {
		t.Fatal("manual include lost to duplicate decision")
	}
}

func TestPageSelectionOverlappingJoinsAndStableIDs(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	m := &media.Manifest{Files: []media.File{reviewFixture("002-004", 2, 4, "a"), reviewFixture("003-005", 3, 5, "b")}}
	r := selectionOf(k, m)
	if r.Selected != 2 || !slices.Contains(r.Pages[0].Flags, "overlapping_joined_ranges") || !slices.Contains(r.Pages[1].Flags, "overlapping_joined_ranges") {
		t.Fatalf("overlap: %+v", r)
	}
	m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
	if reordered := selectionOf(k, m); reordered.Pages[0].ID != reviewHash("b") || reordered.Pages[1].ID != reviewHash("a") {
		t.Fatalf("reorder changed IDs: %+v", reordered)
	}
}

func TestPageSelectionLegacyAndUnavailableAnalysis(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	f := media.File{Path: "originals/old-page-name.png", Blob: "sha256-" + reviewHash("abc"), Meta: map[string]any{digestMeta: reviewHash("abc")}}
	f.Meta, _ = media.WithPageAnalysis(f.Meta, media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: reviewHash("abc"), Status: "unsupported", Blank: "unknown", Confidence: "unknown"})
	r := selectionOf(k, &media.Manifest{Files: []media.File{f}})
	if r.Pages[0].ID != reviewHash("abc") || !r.Pages[0].Included || !slices.Contains(r.Pages[0].Flags, "analysis_unsupported") {
		t.Fatalf("legacy key: %+v", r)
	}
}

func TestArchivePageNamePlanPreservesGapsAndSources(t *testing.T) {
	taken := map[string]bool{}
	for _, test := range []struct{ source, want string }{{"1.png", "1"}, {"2.png", "2"}, {"10.png", "10"}, {"11.jpg", "11"}, {"credits.png", "credits"}, {"001.webp", "001"}, {"credits.jpg", "credits (2)"}} {
		got, _, _ := plannedPageName(test.source, taken)
		if got != test.want {
			t.Fatalf("%s -> %s, want %s", test.source, got, test.want)
		}
	}
}

func TestPageSelectionRetainsOneBlankForNonblockingPublication(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	f := reviewFixture("001", 1, 1, strings.Repeat("a", 64))
	meta := pageMeta(f)
	meta.Analysis = &media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: strings.Repeat("a", 64), Status: "complete", Blank: "blank", Confidence: "exact"}
	writePageMeta(&f, meta)
	r := selectionOf(k, &media.Manifest{Files: []media.File{f}})
	if r.Selected != 1 || r.Pages[0].Decision.Reason != "empty_selection_fallback" {
		t.Fatalf("blank-only fallback: %+v", r)
	}
}

func TestPageSelectionSegmentGuardKeepsPriorSelection(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	a, b := reviewFixture("001", 1, 1, "a"), reviewFixture("002", 2, 2, "a")
	meta := pageMeta(b)
	no := false
	meta.Included = &no
	writePageMeta(&b, meta)
	m := &media.Manifest{Files: []media.File{a, b}}
	guardSegmentedPages(k, m, true)
	r := selectionOf(k, m)
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

func TestPageNoticesForRemovalsAndArchiveReorder(t *testing.T) {
	k := &media.Kind{Publish: &media.Publish{Pages: "originals/{name}"}}
	files := []media.File{reviewFixture("001", 1, 1, "a"), reviewFixture("002-003", 2, 3, "b"), reviewFixture("004", 2, 2, "c"), reviewFixture("005", 5, 5, "d"), reviewFixture("006", 6, 6, "a")}
	blank := pageMeta(files[3])
	blank.Analysis = &media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: reviewHash("d"), Status: "complete", Blank: "blank", Confidence: "exact"}
	writePageMeta(&files[3], blank)
	// An archive listed "10.png" before "1.png"; natural order put 1.png first.
	for i, upload := range []int{2, 1, 3, 4, 5} {
		meta := pageMeta(files[i])
		meta.ArchiveSHA256, meta.SourceIndex, meta.UploadIndex = "zip", i+1, upload
		writePageMeta(&files[i], meta)
	}
	m := &media.Manifest{Files: files}
	notices := noticesOf(k, m)
	if len(notices) != 2 || notices[0].Kind != "removed" || notices[1].Kind != "reordered" {
		t.Fatalf("notices: %+v", notices)
	}
	reasons := map[string]string{}
	for _, p := range notices[0].Pages {
		reasons[p.ID] = p.Reason
	}
	if len(reasons) != 3 || reasons[reviewHash("c")] != "spread" || reasons[reviewHash("d")] != "blank" || reasons[reviewHash("a")] != "duplicate" {
		t.Fatalf("removed reasons: %+v", notices[0].Pages)
	}
	if p := notices[1].Pages; len(p) != 5 || p[0].From != 2 || p[0].To != 1 || p[1].From != 1 || p[1].To != 2 {
		t.Fatalf("reorder: %+v", p)
	}
	removed := notices[0].ID
	meta := pageMeta(m.Files[4])
	meta.Mode = "include"
	writePageMeta(&m.Files[4], meta)
	after := noticesOf(k, m)
	if after[0].Kind != "removed" || len(after[0].Pages) != 2 || after[0].ID == removed {
		t.Fatalf("a restore must change the removed notice, never reuse a dismissal: %+v", after)
	}
}

func TestPositionalPageNames(t *testing.T) {
	for _, c := range []struct {
		position, count int
		ext, want       string
	}{{1, 9, ".jpg", "01.jpg"}, {10, 10, ".png", "10.png"}, {7, 100, ".webp", "007.webp"}, {120, 1200, ".jpg", "0120.jpg"}} {
		if got := media.PositionalName(c.position, c.count, c.ext); got != c.want {
			t.Errorf("PositionalName(%d, %d) = %s, want %s", c.position, c.count, got, c.want)
		}
	}
}
