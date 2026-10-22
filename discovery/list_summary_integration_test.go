package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
)

func TestListingSummaryCountsSelectionNotPage(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts", "yaoi")
	f.seed(
		item{n: 1, age: time.Hour, titles: map[string]string{"en": "Visible"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, age: time.Hour, groups: []string{"big-breasts"}, titles: map[string]string{"en": "Filtered"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, age: time.Hour, groups: []string{"yaoi"}, titles: map[string]string{"en": "Other filtered"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, age: time.Hour, groups: []string{"big-breasts", "yaoi"}, titles: map[string]string{"en": "Both"}, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
		item{n: 5, age: time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, titles: map[string]string{"en": "Premium"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 6, age: time.Hour, groups: []string{"big-breasts"}, rule: `{"level":"tier","tiers":["premium"]}`, titles: map[string]string{"en": "Filtered premium"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 7, titles: map[string]string{"en": "Gallery 7"}, age: time.Hour, groups: []string{"big-breasts"}, deleted: true, versions: []ver{{langs: []string{"en"}}}},
		item{n: 8, titles: map[string]string{"en": "Gallery 8"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}, future: true}}},
	)
	preferences, err := json.Marshal(ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids["yaoi"], Visible: false})})
	if err != nil {
		t.Fatal(err)
	}
	guest := string(preferences)
	var out ContentPage
	w := f.guestCall(http.MethodGet, "main.test", "", q("/contents", "kind", "gallery", "summary", "true", "limit", "1"), guest, nil, &out)
	if w.Code != http.StatusOK || out.Summary == nil {
		t.Fatalf("summary: %d %s", w.Code, w.Body)
	}
	counts := filteredCounts(ReleaseDay{FilteredEntities: out.Summary.FilteredEntities})
	if out.Summary.FilteredCount != 4 || out.Summary.LockedCount != 1 || counts["big-breasts"] != 3 || counts["yaoi"] != 2 || len(out.Contents) != 1 || out.NextCursor == "" {
		t.Fatalf("selection summary %+v, counts=%v, page=%+v", out.Summary, counts, out)
	}
	var next ContentPage
	w = f.guestCall(http.MethodGet, "main.test", "", q("/contents", "kind", "gallery", "summary", "true", "limit", "1", "cursor", out.NextCursor), guest, nil, &next)
	if w.Code != http.StatusOK || next.Summary != nil {
		t.Fatalf("later page repeats or fails summary: %d %s", w.Code, w.Body)
	}
}

func TestListingSummaryHonorsRequiredAndHardDomainRules(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts", "yaoi", "restricted")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Gallery 1"}, age: time.Hour, groups: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Gallery 2"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Gallery 3"}, age: time.Hour, groups: []string{"big-breasts", "restricted"}, versions: []ver{{langs: []string{"en"}}}},
	)
	for _, tc := range []struct {
		host string
		want int
	}{{"gay.test", 1}, {"clean.test", 2}} {
		var out ContentPage
		status, body := f.call(http.MethodGet, tc.host, "", q("/contents", "kind", "gallery", "summary", "true"), nil, &out)
		if status != http.StatusOK || out.Summary == nil || out.Summary.FilteredCount != tc.want {
			t.Fatalf("%s summary: %d %s", tc.host, status, body)
		}
	}
}

func TestListingSummaryKeepsPopularitySelection(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Gallery 1"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Gallery 2"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Gallery 3"}, age: time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}},
	)
	selected, err := f.svc.compile(f.ctx, Reader{Host: "main.test"}, Filter{Kind: "gallery"})
	if err != nil {
		t.Fatal(err)
	}
	// The actual ranker uses this join shape for rolling and historical windows.
	ranking := order{raw: SortPopular + ":gallery:historic", expr: "activity.score", join: "JOIN unnest(@summary_ranked::text[], @summary_scores::float8[]) AS activity(content_id, score) ON activity.content_id=f.content_id", args: pgx.NamedArgs{"summary_ranked": []string{f.cid(1), f.cid(3)}, "summary_scores": []float64{4, 2}}}
	out, err := f.svc.listingSummary(f.ctx, Filter{Kind: "gallery"}, selected, ranking, nil)
	if err != nil || out.FilteredCount != 1 || out.LockedCount != 1 {
		t.Fatalf("popularity summary %+v: %v", out, err)
	}
}

func TestSearchSummaryMatchesKeywordAndEmptySelection(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts")
	f.seed(
		item{n: 1, age: time.Hour, titles: map[string]string{"en": "Moon Story"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, age: time.Hour, titles: map[string]string{"en": "Moon Secret"}, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, age: time.Hour, titles: map[string]string{"en": "Unrelated"}, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	f.docs(doc("gallery", f.cid(1), "en", "Moon Story"), doc("gallery", f.cid(2), "en", "Moon Secret"), doc("gallery", f.cid(3), "en", "Unrelated"))
	for _, tc := range []struct {
		query string
		want  int
	}{{"Moon", 1}, {"Secret", 1}, {"NoSuchKeyword", 0}} {
		var out ContentPage
		status, body := f.call(http.MethodGet, "main.test", "", q("/search", "kind", "gallery", "q", tc.query, "summary", "true"), nil, &out)
		if status != http.StatusOK || out.Summary == nil || out.Summary.FilteredCount != tc.want {
			t.Fatalf("%s keyword summary: %d %s", tc.query, status, body)
		}
	}
}

func TestListingSummarySettlesIncompleteEntitlements(t *testing.T) {
	billing := &releaseBilling{held: map[string]bool{}}
	f := newFixtureBilling(t, nil, "", siteConfig(), billing)
	f.group("tag", "big-breasts")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Gallery 1"}, age: time.Hour, groups: []string{"big-breasts"}, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Gallery 2"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
	)
	out, err := f.svc.List(f.ctx, ListOptions{Reader: Reader{Host: "main.test", Actor: access.Actor{ID: "reader"}}, Filter: Filter{Kind: "gallery"}, Summary: true})
	if err != nil || out.Summary == nil || out.Summary.FilteredCount != 1 || out.Summary.LockedCount != 1 {
		t.Fatalf("settled summary %+v: %v", out.Summary, err)
	}
}

func TestListingSummaryWorksWithOnePoolConnection(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts")
	f.seed(item{n: 1, titles: map[string]string{"en": "Gallery 1"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}})
	cfg := f.pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.svc.o.Pool = pool
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	out, err := f.svc.List(ctx, ListOptions{Reader: Reader{Host: "main.test"}, Filter: Filter{Kind: "gallery"}, Summary: true})
	if err != nil || out.Summary == nil || !reflect.DeepEqual(out.Contents, []Card{}) || out.Summary.FilteredCount != 1 {
		t.Fatalf("one-connection summary %+v: %v", out, err)
	}
}
