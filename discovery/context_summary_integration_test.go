package discovery

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

func TestFavoritesSummaryBelongsToCallerBeforePagination(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts", "restricted")
	f.seed(
		item{n: 1, age: time.Hour, titles: map[string]string{"en": "Free"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, age: time.Hour, titles: map[string]string{"en": "Filtered favorite"}, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, age: time.Hour, titles: map[string]string{"en": "Premium favorite"}, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, age: time.Hour, titles: map[string]string{"en": "Someone else's favorite"}, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 5, age: time.Hour, titles: map[string]string{"en": "Hard restricted"}, groups: []string{"restricted", "big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	for _, n := range []int{1, 2, 3, 5} {
		f.exec(`INSERT INTO {s}.content_favorites (tenant_id,user_id,content_kind,content_id,value,revision) VALUES ($1,'reader','gallery',$2,1,1)`, tenant, f.cid(n))
	}
	f.exec(`INSERT INTO {s}.content_favorites (tenant_id,user_id,content_kind,content_id,value,revision) VALUES ($1,'other','gallery',$2,1,1)`, tenant, f.cid(4))
	var out InteractionPage
	status, body := f.call(http.MethodGet, "clean.test", "reader", "/me/favorites?kind=gallery&summary=true&limit=1", nil, &out)
	if status != http.StatusOK || out.Summary == nil || out.Summary.FilteredCount != 1 || out.Summary.LockedCount != 1 || len(out.Items) != 1 || out.NextCursor == "" {
		t.Fatalf("caller-scoped summary: %d %s", status, body)
	}
	var freeOnly InteractionPage
	status, body = f.call(http.MethodGet, "clean.test", "reader", "/me/favorites?kind=gallery&summary=true&limit=1&mode=hide", nil, &freeOnly)
	if status != http.StatusOK || len(freeOnly.Items) != 1 || freeOnly.Items[0].Content == nil || freeOnly.Items[0].Content.ID != f.cid(1) || freeOnly.Summary == nil || freeOnly.Summary.LockedCount != 0 {
		t.Fatalf("hide mode must apply before the page limit: %d %s", status, body)
	}
	var next InteractionPage
	status, body = f.call(http.MethodGet, "clean.test", "reader", q("/me/favorites", "kind", "gallery", "summary", "true", "cursor", out.NextCursor), nil, &next)
	if status != http.StatusOK || next.Summary != nil {
		t.Fatalf("cursor must not repeat counts: %d %s", status, body)
	}
	var other InteractionPage
	status, body = f.call(http.MethodGet, "clean.test", "other", "/me/favorites?kind=gallery&summary=true", nil, &other)
	if status != http.StatusOK || other.Summary == nil || other.Summary.FilteredCount != 1 || other.Summary.LockedCount != 0 {
		t.Fatalf("other caller: %d %s", status, body)
	}
}

func TestRecommendationSummaryUsesOnlyConsideredCandidates(t *testing.T) {
	f := newFixture(t, nil, "")
	f.group("tag", "big-breasts")
	f.seed(
		item{n: 1, age: time.Hour, titles: map[string]string{"en": "Candidate"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, age: time.Hour, titles: map[string]string{"en": "Filtered candidate"}, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, age: time.Hour, titles: map[string]string{"en": "Outside candidates"}, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	o := RecommendationOptions{Reader: Reader{Host: "main.test"}, Filter: Filter{Kind: "gallery"}, Summary: true}
	selected, err := f.svc.compile(f.ctx, o.Reader, o.Filter)
	if err != nil {
		t.Fatal(err)
	}
	a := admission{s: f.svc, q: selected, picked: map[string]pick{}, candidates: map[string]struct{}{}}
	refs := []contentref.ContentRef{contentref.New(tenant, "gallery", f.cid(1)), contentref.New(tenant, "gallery", f.cid(2))}
	if _, err = a.admit(f.ctx, refs); err != nil {
		t.Fatal(err)
	}
	out, err := a.cards(f.ctx, []Hit{{ContentRef: refs[0]}}, o)
	if err != nil || out.Summary == nil || out.Summary.FilteredCount != 1 || !out.Summary.Truncated || len(out.Contents) != 1 {
		t.Fatalf("bounded candidates summary: %+v, %v", out, err)
	}
}
