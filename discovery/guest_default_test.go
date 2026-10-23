package discovery

import (
	"github.com/open-rails/contentkit/kinds"
	"net/http"
	"testing"
)

func TestGuestDefaultUsesCurrentDomainAndSurvivesRegistration(t *testing.T) {
	cfg := siteConfig()
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}, Entities: []string{"tag"}}
	cfg.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"gallery", "blog"}, Domains: []kinds.Domain{
		{Name: "main", Hosts: []string{"a.test"}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "yaoi"}, Visible: false}}},
		{Name: "b", Hosts: []string{"b.test"}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "yaoi"}, Visible: true}}},
	}}
	f := newFixtureConfig(t, nil, "", cfg)
	f.entity("tag", "yaoi")
	id := f.ids["yaoi"]
	header := `{"filters":[{"entity_id":"` + id + `","default":true}]}`
	check := func(host, user, header string, visible bool) {
		t.Helper()
		var got ContentFilters
		w := f.guestCall(http.MethodGet, host, user, "/content-filters", header, nil, &got)
		if w.Code != http.StatusOK || len(got.Filters) != 1 || got.Filters[0].Visible != visible || got.Filters[0].Overridden {
			t.Fatalf("%s %s: %d %+v", host, user, w.Code, got)
		}
	}
	check("a.test", "", header, false)
	check("b.test", "", header, true)
	reset := true
	init := ContentFiltersInitialization{AccountID: "new", Filters: []GuestFilterSetting{{EntityID: id, Default: &reset}}}
	if status, _ := f.call(http.MethodPost, "a.test", "new", "/me/content-filters/initialize", init, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	check("b.test", "new", "", true)
	// Defaults are evaluated on each read, never pinned to the registration host.
	f.svc.o.Kinds.ContentFilters.Domains[0].Filters[0].Visible = true
	check("a.test", "new", "", true)
	for _, bad := range []GuestFilterSetting{{EntityID: id}, {EntityID: id, Default: boolPtr(false)}, {EntityID: id, Visible: boolPtr(false), Default: boolPtr(true)}} {
		if _, err := bad.value(); err == nil {
			t.Fatalf("invalid choice accepted%+v", bad)
		}
	}
}
func boolPtr(v bool) *bool { return &v }
