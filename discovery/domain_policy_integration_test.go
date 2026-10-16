package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
)

func TestDomainPolicyBoundariesAndPreferences(t *testing.T) {
	cfg := siteConfig()
	locked, toggle := false, true
	cfg.Domains = []kinds.Domain{
		{Name: "main", Hosts: []string{"main.test"}, Filters: []kinds.ContentFilter{
			{Group: kinds.GroupRef{Kind: "tag", Slug: "big-breasts"}},
			{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true},
		}},
		{Name: "gay", Hosts: []string{"gay.test"}, RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}, Filters: []kinds.ContentFilter{
			{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true},
			{Group: kinds.GroupRef{Kind: "tag", Slug: "gore"}, Visible: false, UserToggle: &locked},
		}},
		// An explicit fixed rule remains hard after its marker is renamed.
		{Name: "safe", Hosts: []string{"safe.test"}, Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false, UserToggle: &locked}}},
		{Name: "other", Hosts: []string{"other.test"}, Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "gore"}, Visible: true}}},
		// Even an explicit true cannot expose an internal classification.
		{Name: "implicit", Hosts: []string{"implicit.test"}, Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false, UserToggle: &toggle}}},
	}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "big-breasts", "yaoi", "gore", "restricted")
	internal := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(f.ids["restricted"]), Internal: &internal}}); err != nil {
		t.Fatal(err)
	}
	for i, gs := range [][]string{nil, {"big-breasts"}, {"yaoi"}, {"gore"}, {"restricted"}, {"yaoi", "gore"}, {"yaoi", "restricted"}} {
		f.seed(item{n: i + 1, titles: map[string]string{"en": "Story"}, age: time.Hour, groups: gs, versions: []ver{{langs: []string{"en"}}}})
		f.docs(doc("gallery", f.cid(i+1), "en", "Story"))
	}
	assertList := func(host, user string, want ...int) {
		t.Helper()
		ids := make([]string, 0, len(want))
		for _, n := range want {
			ids = append(ids, f.cid(n))
		}
		slices.Sort(ids)
		for _, path := range []string{"/contents?kind=gallery", "/search?kind=gallery&q=Story"} {
			got, _ := f.list(host, user, path)
			slices.Sort(got)
			if !slices.Equal(got, ids) {
				t.Fatalf("%s on %s as %s: %v want %v", path, host, user, got, ids)
			}
		}
	}
	for _, user := range []string{"", "reader"} {
		assertList("main.test", user, 1, 3, 4, 5, 6, 7)
		assertList("gay.test", user, 3, 7)
		assertList("safe.test", user, 1, 2, 3, 4, 6)
		assertList("implicit.test", user, 1, 2, 3, 4, 6)
	}
	for _, host := range []string{"gay.test", "safe.test", "implicit.test"} {
		var settings ContentFilters
		if status, _ := f.call(http.MethodGet, host, "", "/content-filters", nil, &settings); status != http.StatusOK || len(settings.Filters) != 0 {
			t.Fatalf("locked/internal/required controls exposed on %s: %d %+v", host, status, settings)
		}
	}
	// An old account choice and forged guest choice cannot unlock a fixed rule.
	f.exec(`INSERT INTO {s}.content_filter_prefs (tenant_id,user_id,overrides) VALUES ($1,'forged',$2)`, tenant, `{"`+f.ids["gore"]+`":"show","`+f.ids["restricted"]+`":"show"}`)
	assertList("gay.test", "forged", 3, 7)
	assertList("safe.test", "forged", 1, 2, 3, 4, 6)
	assertList("implicit.test", "forged", 1, 2, 3, 4, 6)
	header := `{"filters":[{"group_id":"` + f.ids["gore"] + `","visible":true},{"group_id":"` + f.ids["restricted"] + `","visible":true}]}`
	var page ContentPage
	if w := f.guestCall(http.MethodGet, "safe.test", "", "/contents?kind=gallery", header, nil, &page); w.Code != http.StatusOK || len(page.Contents) != 5 {
		t.Fatalf("guest escaped internal rule: %d %s", w.Code, w.Body)
	}
	// Fixed rules also constrain direct HTTP reads for staff; unlisted rules do not.
	for _, tc := range []struct {
		host      string
		n, status int
	}{{"safe.test", 5, http.StatusNotFound}, {"gay.test", 6, http.StatusNotFound}, {"gay.test", 1, http.StatusNotFound}, {"main.test", 5, http.StatusOK}, {"other.test", 2, http.StatusOK}} {
		for _, user := range []string{"", "staff"} {
			r := httptest.NewRequest(http.MethodGet, "/contents/"+f.cid(tc.n), nil)
			r.Host = tc.host
			if user != "" {
				r = testauth.As(r, access.Actor{ID: user})
			}
			w := httptest.NewRecorder()
			f.cat.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("direct %s %d: %d %s", tc.host, tc.n, w.Code, w.Body)
			}
		}
	}
	// Explicit management remains available across domains after authorization.
	r := testauth.As(httptest.NewRequest(http.MethodGet, "/manage/gallery", nil), access.Actor{ID: "staff"})
	r.Host = "safe.test"
	w := httptest.NewRecorder()
	f.cat.Handler().ServeHTTP(w, r)
	var managed []catalog.Content
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &managed) != nil || len(managed) != 7 {
		t.Fatalf("management constrained by public domain: %d %s", w.Code, w.Body)
	}
	var sitemap SitemapPage
	if status, _ := f.call(http.MethodGet, "safe.test", "", "/sitemap/contents?kind=gallery", nil, &sitemap); status != http.StatusOK || len(sitemap.Entries) != 5 {
		t.Fatalf("hard filter in sitemap: %d %+v", status, sitemap)
	}

	// Saving another domain cannot erase main's choice and vice versa.
	save := func(host string, filters []FilterSetting) {
		t.Helper()
		if status, _ := f.call(http.MethodPut, host, "reader", "/me/content-filters", ContentFiltersUpdate{Filters: filters}, nil); status != http.StatusOK {
			t.Fatalf("save %s: %d", host, status)
		}
	}
	save("main.test", []FilterSetting{{GroupID: f.ids["big-breasts"], Visible: true}})
	save("other.test", []FilterSetting{{GroupID: f.ids["gore"], Visible: false}})
	assertList("main.test", "reader", 1, 2, 3, 4, 5, 6, 7)
	assertList("other.test", "reader", 1, 2, 3, 5, 7)
	save("main.test", nil)
	assertList("main.test", "reader", 1, 3, 4, 5, 6, 7)
	assertList("other.test", "reader", 1, 2, 3, 5, 7)
	// Internal entities are absent from cards and cannot be used as public selectors.
	var detail map[string]any
	if status, _ := f.call(http.MethodGet, "main.test", "", "/contents?kind=gallery", nil, &detail); status != http.StatusOK {
		t.Fatal(status)
	}
	raw, _ := json.Marshal(detail)
	if strings.Contains(string(raw), f.ids["restricted"]) {
		t.Fatal("internal entity exposed in content cards")
	}
	for _, field := range []string{"groups", "exclude_groups", "owner_group"} {
		if status, _ := f.call(http.MethodGet, "main.test", "", "/contents?"+field+"="+f.ids["restricted"], nil, nil); status != http.StatusBadRequest {
			t.Fatalf("internal selector %s: %d", field, status)
		}
	}
	// A renamed hard marker must not silently widen the domain.
	renamed := "renamed-restricted"
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(f.ids["restricted"]), Slug: &renamed}}); err != nil {
		t.Fatal(err)
	}
	assertList("safe.test", "")
	r = httptest.NewRequest(http.MethodGet, "/contents/"+f.cid(1), nil)
	r.Host = "safe.test"
	w = httptest.NewRecorder()
	f.cat.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing hard marker widened domain: %d %s", w.Code, w.Body)
	}

}
