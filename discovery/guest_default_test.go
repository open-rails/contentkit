package discovery

import (
	"net/http"
	"testing"

	"github.com/open-rails/contentkit/kinds"
)

func TestGuestDefaultUsesCurrentDomainAndSurvivesRegistration(t *testing.T) {
	cfg := siteConfig()
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}}
	cfg.Domains = []kinds.Domain{
		{Name: "a", Hosts: []string{"a.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: false}}}, "blog": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true}}}}},
		{Name: "b", Hosts: []string{"b.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true}}}, "blog": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true}}}}},
	}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "yaoi")
	id := f.ids["yaoi"]
	header := `{"legacy_filters":[{"entity_id":"` + id + `","visible":false}],"kinds":{"gallery":[{"entity_id":"` + id + `","default":true}]}}`
	check := func(host, user, header string, visible bool) {
		t.Helper()
		var got ContentFilters
		w := f.guestCall(http.MethodGet, host, user, "/content-filters", header, nil, &got)
		if w.Code != http.StatusOK || got.Kinds["gallery"][0].Visible != visible || got.Kinds["gallery"][0].Overridden || got.Kinds["blog"][0].Visible || !got.Kinds["blog"][0].Overridden {
			t.Fatalf("%s %s: %d %+v", host, user, w.Code, got)
		}
	}
	check("a.test", "", header, false)
	check("b.test", "", header, true)
	reset := true
	init := ContentFiltersInitialization{AccountID: "new", Kinds: map[string][]GuestFilterSetting{"gallery": {{EntityID: id, Default: &reset}}}, LegacyFilters: []FilterSetting{{EntityID: id, Visible: false}}}
	if status, _ := f.call(http.MethodPost, "a.test", "new", "/me/content-filters/initialize", init, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	check("b.test", "new", "", true)
	// A later default change must be read dynamically; the reset did not pin false.
	cfg.Domains[0].Kinds["gallery"].Filters[0].Visible = true
	check("a.test", "new", "", true)
	for _, bad := range []GuestFilterSetting{{EntityID: id}, {EntityID: id, Default: boolPtr(false)}, {EntityID: id, Visible: boolPtr(false), Default: boolPtr(true)}} {
		if _, err := bad.value(); err == nil {
			t.Fatalf("invalid choice accepted: %+v", bad)
		}
	}
}

func boolPtr(v bool) *bool { return &v }
