package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
)

func TestContentFilterEntrypointDefaults(t *testing.T) {
	cfg := siteConfig()
	cfg.Domains = []kinds.Domain{{Name: "main", Hosts: []string{"main.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: cfg.Domains[0].Kinds["gallery"].Filters}, "video": {Filters: cfg.Domains[0].Kinds["gallery"].Filters}}}, {Name: "alternate", Hosts: []string{"alternate.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: []kinds.ContentFilter{
		{Group: cfg.Domains[0].Kinds["gallery"].Filters[0].Group, Visible: true},
		{Group: cfg.Domains[0].Kinds["gallery"].Filters[1].Group, Visible: false},
	}}, "video": {Filters: []kinds.ContentFilter{
		{Group: cfg.Domains[0].Kinds["gallery"].Filters[0].Group, Visible: true},
		{Group: cfg.Domains[0].Kinds["gallery"].Filters[1].Group, Visible: false},
	}}}}, {Name: "strict", Hosts: []string{"strict.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}}, "video": {RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}}}},
	}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "big-breasts", "yaoi")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Story one"}, age: 4 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Story two"}, age: 3 * time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Story three"}, age: 2 * time.Hour, groups: []string{"yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Story four"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	for _, n := range []int{1, 2, 3, 4} {
		f.docs(doc("gallery", f.cid(n), "en", "Story"))
	}
	assertList := func(host, user string, want ...int) {
		t.Helper()
		ids := make([]string, 0, len(want))
		for _, n := range want {
			ids = append(ids, f.cid(n))
		}
		slices.Sort(ids)
		for _, path := range []string{q("/contents", "kind", "gallery"), q("/search", "kind", "gallery", "q", "Story")} {
			got, _ := f.list(host, user, path)
			slices.Sort(got)
			if !slices.Equal(got, ids) {
				t.Fatalf("%s on %s as %q: %v, want %v", path, host, user, got, ids)
			}
		}
	}
	settings := func(host string, defaultBig, defaultYaoi, visibleBig, visibleYaoi, overridden bool) {
		t.Helper()
		var got ContentFilters
		if status, _ := f.call(http.MethodGet, host, "reader", "/content-filters", nil, &got); status != http.StatusOK {
			t.Fatalf("settings %s: %d", host, status)
		}
		if len(got.Kinds["gallery"]) != 2 {
			t.Fatalf("settings %s: %+v", host, got)
		}
		if a, b := got.Kinds["gallery"][0], got.Kinds["gallery"][1]; a.DefaultVisible != defaultBig || b.DefaultVisible != defaultYaoi ||
			a.Visible != visibleBig || b.Visible != visibleYaoi || a.Overridden != overridden || b.Overridden != overridden {
			t.Fatalf("settings %s: %+v", host, got)
		}
	}
	for _, user := range []string{"", "reader"} {
		assertList("main.test", user, 1, 3)
		assertList("ALTERNATE.TEST:8443", user, 1, 2, 4)
		assertList("unknown.test:8080", user, 1, 3)
		assertList("strict.test", user, 3)
	}
	settings("main.test", false, true, false, true, false)
	settings("ALTERNATE.TEST:8443", true, false, true, false, false)
	settings("unknown.test:8080", false, true, false, true, false)

	// Cached anonymous totals remain per site, irrespective of request order
	// or a signed-in reader's preferences.
	checkTotals := func() {
		t.Helper()
		for _, tc := range []struct {
			host  string
			count int64
		}{
			{"main.test", 2}, {"ALTERNATE.TEST:8443", 3}, {"strict.test", 1},
			{"unknown.test:8080", 2}, {"alternate.test", 3}, {"main.test", 2},
		} {
			r := httptest.NewRequest(http.MethodGet, "/catalog/totals?kinds=gallery", nil)
			r.Host = tc.host
			w := httptest.NewRecorder()
			f.cat.Handler().ServeHTTP(w, r)
			var out catalog.Totals
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Kinds["gallery"].Public != tc.count {
				t.Fatalf("totals %s: %d %s; want %d", tc.host, w.Code, w.Body, tc.count)
			}
		}
	}
	checkTotals()
	update := ContentFiltersUpdate{Kinds: map[string][]FilterSetting{"gallery": []FilterSetting{{EntityID: f.ids["big-breasts"], Visible: true}, {EntityID: f.ids["yaoi"], Visible: true}}}}
	if status, _ := f.call(http.MethodPut, "alternate.test", "reader", "/me/content-filters", update, nil); status != http.StatusOK {
		t.Fatalf("save overrides: %d", status)
	}
	assertList("main.test", "reader", 1, 2, 3, 4)
	assertList("alternate.test", "reader", 1, 2, 3, 4)
	settings("main.test", false, true, true, true, true)
	settings("alternate.test", true, false, true, true, true)
	// An explicit false also wins over a site's true default.
	update.Kinds["gallery"][0].Visible = false
	if status, _ := f.call(http.MethodPut, "main.test", "reader", "/me/content-filters", update, nil); status != http.StatusOK {
		t.Fatalf("hide override: %d", status)
	}
	assertList("alternate.test", "reader", 1, 3)
	checkTotals()
	if status, _ := f.call(http.MethodPut, "alternate.test", "reader", "/me/content-filters", ContentFiltersUpdate{Kinds: map[string][]FilterSetting{"gallery": {}}}, nil); status != http.StatusOK {
		t.Fatalf("reset: %d", status)
	}
	assertList("main.test", "reader", 1, 3)
	assertList("alternate.test", "reader", 1, 2, 4)
	settings("alternate.test", true, false, true, false, false)

	// Preferences remain soft. Direct reads ignore them, while the existing
	// explicit hard site requirement continues to apply.
	for _, host := range []string{"main.test", "alternate.test"} {
		for _, n := range []int{1, 2, 3, 4} {
			r := httptest.NewRequest(http.MethodGet, "/contents/"+f.cid(n), nil)
			r.Host = host
			w := httptest.NewRecorder()
			f.cat.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("soft-filtered direct read %s/%d: %d %s", host, n, w.Code, w.Body)
			}
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/contents/"+f.cid(1), nil)
	r.Host = "strict.test"
	w := httptest.NewRecorder()
	f.cat.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("hard site requirement ignored: %d %s", w.Code, w.Body)
	}
}
