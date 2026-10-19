package versions

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/resolve"
)

func chapterHash(n int) string { return fmt.Sprintf("%064x", n) }

func chapterPages(ns ...int) []page {
	pages := make([]page, len(ns))
	for i, n := range ns {
		pages[i] = page{Name: fmt.Sprintf("page-%d", n), SourceHash: chapterHash(n)}
	}
	return pages
}

func TestChapterStartsFollowSourceHashes(t *testing.T) {
	starts := []segment{{Position: 0, StartPage: chapterHash(1)}, {Position: 1, StartPage: chapterHash(3)}}
	for _, tc := range []struct {
		name  string
		pages []page
		cut   int
	}{
		{"original", chapterPages(1, 2, 3, 4), 2},
		{"insert before second start", chapterPages(1, 2, 5, 3, 4), 3},
		{"delete unmarked page", chapterPages(1, 3, 4), 1},
		{"reorder unmarked pages", chapterPages(1, 5, 2, 3, 4), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChapterStarts(starts, tc.pages)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Start != 0 || got[0].End != tc.cut || got[1].Start != tc.cut || got[1].End != len(tc.pages) {
				t.Fatalf("derived ranges: %+v", got)
			}
			if starts[0].End != 0 || starts[1].Start != 0 {
				t.Fatal("resolver changed stored chapter inputs")
			}
		})
	}
}

func TestChapterStartsRefuseAmbiguousOrMissingHashes(t *testing.T) {
	starts := []segment{{Position: 0, StartPage: chapterHash(1)}, {Position: 1, StartPage: chapterHash(3)}}
	for _, pages := range [][]page{chapterPages(1, 2), chapterPages(3, 2, 1), chapterPages(1, 3, 3)} {
		if _, err := resolveChapterStarts(starts, pages); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad source order %v: %v", pages, err)
		}
	}
	if _, err := resolveChapterStarts([]segment{{StartPage: chapterHash(1)}, {StartPage: chapterHash(1)}}, chapterPages(1, 2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate chapter anchor: %v", err)
	}
	// Duplicated images that are not used as a chapter marker are harmless.
	if _, err := resolveChapterStarts(starts, chapterPages(1, 2, 2, 3)); err != nil {
		t.Fatalf("unanchored duplicate: %v", err)
	}
}

func TestChapterStartsKeepPublicationReleaseBoundaries(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	starts := []segment{{Position: 0, StartPage: chapterHash(1)}, {Position: 1, StartPage: chapterHash(3), ReleaseAt: &future}}
	for _, source := range [][]page{chapterPages(1, 2, 3, 4), chapterPages(1, 2, 5, 3, 4)} {
		segs, err := resolveChapterStarts(starts, source)
		if err != nil {
			t.Fatal(err)
		}
		d := decision{res: resolve.Resolution{Visible: true, Accessible: true}, segs: segs, pages: len(source), now: now}
		if d.whole() || d.shows(segs[1].Start+1) || !d.shows(segs[0].End) {
			t.Fatalf("unreleased page/download crossed snapshot boundary: %+v", segs)
		}
		if freeEnd(segs, 1, len(source)) != segs[1].Start {
			t.Fatal("chapter preview boundary did not follow its snapshot")
		}
	}
	// A new manifest's index must never authorize an older published page.
	old, _ := resolveChapterStarts(starts, chapterPages(1, 2, 3, 4))
	if released(old, 4, now) != 2 {
		t.Fatal("older publication gained newly shifted pages")
	}
}

func TestChapterStartsReadLegacySnapshotSourceHash(t *testing.T) {
	hash := chapterHash(1)
	legacy := []page{{Name: "unchanged-reader-name", Source: "sha256-" + hash + ".edit"}}
	got, err := resolveChapterStarts([]segment{{StartPage: hash}}, legacy)
	if err != nil || len(got) != 1 || got[0].End != 1 {
		t.Fatalf("legacy snapshot source digest: %+v %v", got, err)
	}
	if legacy[0].Name != "unchanged-reader-name" {
		t.Fatal("chapter resolution changed reader URL name")
	}
}

func TestReaderMetadataNormalizesLegacyNamesToHashes(t *testing.T) {
	hash := chapterHash(1)
	legacy := ReaderMeta{SchemaVersion: 1, Anchors: []Anchor{{ID: "a", Page: "001"}}}
	source := []page{{Path: "pages/001.png", Source: "sha256-" + hash + ".edit"}}
	got := legacy.hashPages(source)
	if got.Anchors[0].Page != hash || legacy.Anchors[0].Page != "001" {
		t.Fatalf("legacy normalization changed source or missed fallback: %+v", got)
	}
	if canonical := got.hashPages(source); canonical.Anchors[0].Page != hash {
		t.Fatal("canonical source hash changed")
	}
	source = append(source, page{Name: "another-name", SourceHash: hash})
	if got := legacy.hashPages(source); got.Anchors[0].Page != "" {
		t.Fatal("duplicate source hash got an arbitrary occurrence")
	}
}
