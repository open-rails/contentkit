package discovery

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/popularity"
)

func TestStandingOrderRanksCurrentPeopleInWindow(t *testing.T) {
	f := seeded(t)
	f.seed(item{n: 8, titles: map[string]string{"en": "Old publication, new standing"}, age: 800 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 9, titles: map[string]string{"en": "Boundaries and a stale cached rank"}, age: 30 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 10, titles: map[string]string{"en": "Filtered, standing last month"}, age: 30 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}, groups: []string{"big-breasts"}})
	// Retired open-derived scores and window ratings must never rank.
	f.exec(`UPDATE {s}.content_facets SET popularity='{"score":{"7d":999,"30d":999,"all":999},"rating":{"7d":1,"30d":1}}' WHERE content_id=$1`, f.cid(9))

	from := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -14)
	until := from.AddDate(0, 0, 7)
	in := from.Add(time.Hour)
	// Alpha: two people and a guest, 3:0.
	f.react(1, "a1", 1, in)
	f.react(1, "a2", 1, in)
	f.react(1, "guest:192.0.2.7", 1, in)
	// Old: an 800-day-old publication trends in a recent window, 3:1.
	for _, p := range []string{"o1", "o2", "o4"} {
		f.react(8, p, 1, in)
	}
	f.react(8, "o3", -1, in)
	// Boundaries: one vote exactly at the inclusive start counts; three just
	// before it and three exactly at the exclusive end do not.
	f.react(9, "b0", 1, from)
	for i := range 3 {
		f.react(9, fmt.Sprintf("before-%d", i), 1, from.Add(-time.Microsecond))
		f.react(9, fmt.Sprintf("until-%d", i), 1, until)
	}
	// Charlie: one person's favorite and like are one vote; erased people's
	// restored residue never counts.
	f.favorite(3, "c1", in)
	f.react(3, "c1", 1, in)
	for i := range 3 {
		erased := fmt.Sprintf("erased-%d", i)
		f.exec(`INSERT INTO {s}.content_erased_subjects (tenant_id,actor_id) VALUES ($1,$2)`, tenant, erased)
		f.react(3, erased, 1, in)
		f.favorite(3, erased, in)
	}
	// Bravo stays behind a default-hidden filter; so does the work whose only
	// vote precedes the window.
	f.favorite(2, "b1", in)
	f.react(10, "x1", 1, from.AddDate(0, 0, -10))

	path := func(extra ...string) string {
		return q("/contents", append([]string{"kind", "gallery", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.Format(time.DateOnly)}, extra...)...)
	}
	// Wilson 3:0 > 3:1 > 1:0, the 1:0 tie ordered by id. A counted boundary,
	// stale score, double-counted person or erased vote would reorder these.
	want := []string{f.cid(1), f.cid(8), f.cid(9), f.cid(3)}
	got, page := f.list("main.test", "", path("summary", "true"))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("standing window = %v, want %v", got, want)
	}
	if want := (popularity.RankingInfo{Policy: "standing_wilson", Mode: "standing", RequestedFrom: from.Format(time.RFC3339), RequestedUntil: until.Format(time.RFC3339)}); page.Ranking == nil || *page.Ranking != want {
		t.Fatalf("standing provenance %+v, want %+v", page.Ranking, want)
	}
	for _, c := range page.Contents {
		if c.Ranking != nil {
			t.Fatalf("standing must not report measured participation: %+v", c.Ranking)
		}
	}
	// The summary counts only works this standing selects: Bravo, not the
	// filtered work whose vote falls before the window.
	if page.Summary == nil || page.Summary.FilteredCount != 1 {
		t.Fatalf("window summary %+v", page.Summary)
	}
	// Alpha has two editions: it ranks once, on the edition other sorts pick.
	edition := func(page ContentPage) string {
		var ids []string
		for _, c := range page.Contents {
			if c.ID == f.cid(1) {
				ids = append(ids, c.Version.ID)
			}
		}
		if len(ids) != 1 {
			t.Fatalf("Alpha editions %v", ids)
		}
		return ids[0]
	}
	for _, lang := range []string{"en", "ja"} {
		_, newest := f.list("main.test", "", q("/contents", "kind", "gallery", "language", lang))
		for _, sort := range []string{"popular:30d", "top_rated:30d"} {
			if _, ranked := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", sort, "language", lang)); edition(ranked) != edition(newest) {
				t.Fatalf("%s in %s picks another edition", sort, lang)
			}
		}
	}
	_, month := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular:30d", "summary", "true"))
	if month.Summary == nil || month.Summary.FilteredCount != 2 {
		t.Fatalf("30-day summary %+v", month.Summary)
	}
	// Publication age is a separate constraint.
	if got, _ := f.list("main.test", "", path("published_within", "7d")); !reflect.DeepEqual(got, []string{f.cid(1), f.cid(3)}) {
		t.Fatalf("publication age filter = %v", got)
	}
	first, firstPage := f.list("main.test", "", path("limit", "2"))
	rest, _ := f.list("main.test", "", path("limit", "2", "cursor", firstPage.NextCursor))
	if !reflect.DeepEqual(append(first, rest...), want) {
		t.Fatalf("paged %v then %v", first, rest)
	}

	// Windowed top_rated is reactions-only standing; all-time is the current
	// catalog rating, with no ranking claim.
	if got, page := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "top_rated:30d")); got[0] != f.cid(9) || page.Ranking == nil || page.Ranking.Mode != "standing" {
		t.Fatalf("top_rated:30d = %v %+v", got, page.Ranking)
	}
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,likes) VALUES ($1,'gallery',$2,100)`, tenant, f.cid(8))
	if got, page := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "top_rated")); got[0] != f.cid(8) || page.Ranking != nil {
		t.Fatalf("all-time top_rated = %v %+v", got, page.Ranking)
	}

	// After the explicit retirement no read depends on the cached column.
	if err := catalog.RetireOpenCounters(f.ctx, f.pool, f.schema, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.list("main.test", "", path()); !reflect.DeepEqual(got, want) {
		t.Fatalf("after retirement = %v", got)
	}
	for _, sort := range []string{"popular:7d", "popular:all", "top_rated", "top_rated:365d"} {
		f.list("main.test", "", q("/contents", "kind", "gallery", "sort", sort))
	}
}
