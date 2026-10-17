package discovery

import (
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func TestSharedEntityPreferencesAcrossContentKinds(t *testing.T) {
	cfg := siteConfig()
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}}
	cfg.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"gallery", "blog"}, Domains: []kinds.Domain{
		{Name: "main", Hosts: []string{"main.test"}, Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true}, {Group: kinds.GroupRef{Kind: "tag", Slug: "furry"}, Visible: true}}},
		{Name: "gay", Hosts: []string{"gay.test"}, RequireEntities: map[string][]kinds.GroupRef{"gallery": {{Kind: "tag", Slug: "yaoi"}}}, Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true}, {Group: kinds.GroupRef{Kind: "tag", Slug: "furry"}, Visible: true}}},
	}}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "yaoi", "furry")
	for i, it := range []struct {
		kind string
		tags []string
	}{
		{"gallery", []string{"yaoi", "furry"}}, {"gallery", nil}, {"blog", []string{"yaoi", "furry"}}, {"blog", nil}, {"gallery", []string{"yaoi"}}, {"blog", []string{"yaoi"}},
	} {
		f.seed(item{n: i + 1, kind: it.kind, titles: map[string]string{"en": "Story"}, age: time.Hour, groups: it.tags, versions: []ver{{langs: []string{"en"}}}})
	}
	f.exec(`INSERT INTO {s}.content_filter_prefs(tenant_id,user_id,overrides) VALUES($1,'reader',$2)`, tenant, `{"gallery/`+f.ids["yaoi"]+`":"show","blog/`+f.ids["yaoi"]+`":"hide"}`)
	check := func(host, kind, user string, want ...int) {
		t.Helper()
		got, _ := f.list(host, user, "/contents?kind="+kind)
		expected := []string{}
		for _, n := range want {
			expected = append(expected, f.cid(n))
		}
		slices.Sort(got)
		slices.Sort(expected)
		if !slices.Equal(got, expected) {
			t.Fatalf("%s/%s as%s got%v want%v", host, kind, user, got, expected)
		}
	}
	check("main.test", "gallery", "reader", 2)
	check("main.test", "blog", "reader", 4)
	// A required entity is not offered as a global switch. Its stored hide does
	// not empty required galleries, and untagged blog posts remain eligible.
	check("gay.test", "gallery", "reader", 1, 5)
	check("gay.test", "blog", "reader", 3, 4, 6)
	var controls ContentFilters
	if status, _ := f.call(http.MethodGet, "gay.test", "reader", "/content-filters", nil, &controls); status != http.StatusOK || len(controls.Filters) != 1 || controls.Filters[0].Group.Slug != "furry" || !slices.Equal(controls.AppliesTo, []string{"gallery", "blog"}) {
		t.Fatalf("shared inventory %+v", controls)
	}
	patch := func(user, slug string, visible bool) {
		t.Helper()
		if status, _ := f.call(http.MethodPatch, "main.test", user, "/me/content-filters", ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids[slug], Visible: visible})}, nil); status != http.StatusOK {
			t.Fatal(status)
		}
	}
	patch("reader", "yaoi", true)
	check("main.test", "gallery", "reader", 1, 2, 5)
	check("main.test", "blog", "reader", 3, 4, 6)
	patch("reader", "furry", false)
	check("main.test", "gallery", "reader", 2, 5)
	check("main.test", "blog", "reader", 4, 6)
	check("gay.test", "gallery", "reader", 5)
	check("gay.test", "blog", "reader", 4, 6)
	values, _, err := f.svc.prefs(f.ctx, "reader")
	if err != nil || values["blog/"+f.ids["yaoi"]] != hide || values[readerprefs.Key(f.ids["yaoi"])] != show {
		t.Fatalf("legacy source data lost: %v %v", values, err)
	}
	// Public staff browsing follows reader choices; management still lists all.
	patch("staff", "furry", false)
	check("main.test", "gallery", "staff", 2, 5)
	managed, err := f.cat.Manage(f.ctx, access.Actor{ID: "staff"}, catalog.ManageQuery{Kind: "gallery"})
	if err != nil || len(managed) != 3 {
		t.Fatalf("management filtered: %v %v", managed, err)
	}
	// Optional hiding is a discovery preference; a hard required rule applies
	// to direct URLs even for staff.
	for _, tc := range []struct {
		host      string
		n, status int
	}{{"main.test", 1, http.StatusOK}, {"gay.test", 1, http.StatusOK}, {"gay.test", 2, http.StatusNotFound}} {
		r := httptest.NewRequest(http.MethodGet, "/contents/"+f.cid(tc.n), nil)
		r.Host = tc.host
		r = testauth.As(r, access.Actor{ID: "staff"})
		w := httptest.NewRecorder()
		f.cat.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("direct%d %s:%d", tc.n, tc.host, w.Code)
		}
	}
}

func TestContentFilterPatchesDoNotClobberIndependentChoices(t *testing.T) {
	f := seeded(t)
	f.exec(`INSERT INTO {s}.content_filter_prefs(tenant_id,user_id,overrides,include_deleted) VALUES($1,'tabs',$2,true)`, tenant, `{"unrelated":"hide","gallery/`+f.ids["big-breasts"]+`":"hide"}`)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, choice := range []FilterSetting{{EntityID: f.ids["big-breasts"], Visible: true}, {EntityID: f.ids["yaoi"], Visible: false}} {
		go func() {
			<-start
			results <- f.svc.SetContentFilters(f.ctx, access.Actor{ID: "tabs"}, ContentFiltersUpdate{Filters: filterChoices(choice)})
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	values, include, err := f.svc.prefs(f.ctx, "tabs")
	if err != nil || !include || values[readerprefs.Key(f.ids["big-breasts"])] != show || values[readerprefs.Key(f.ids["yaoi"])] != hide || values["unrelated"] != hide || values["gallery/"+f.ids["big-breasts"]] != hide {
		t.Fatalf("independent edits clobbered: %v include%v err%v", values, include, err)
	}
	reset := true
	if err := f.svc.SetContentFilters(f.ctx, access.Actor{ID: "tabs"}, ContentFiltersUpdate{Filters: []GuestFilterSetting{{EntityID: f.ids["big-breasts"], Default: &reset}}}); err != nil {
		t.Fatal(err)
	}
	values, _, err = f.svc.prefs(f.ctx, "tabs")
	if err != nil {
		t.Fatal(err)
	}
	if _, overridden := readerprefs.Stored(values, f.ids["big-breasts"]); overridden || values[readerprefs.Key(f.ids["yaoi"])] != hide {
		t.Fatalf("reset resurrected legacy or changed another entity: %v", values)
	}
}
