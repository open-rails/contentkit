package discovery

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/kinds"
)

func TestPerKindPreferencesPreserveLegacyAndOtherKinds(t *testing.T) {
	cfg := siteConfig()
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}}
	toggle := kinds.KindPolicy{Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true}}}
	cfg.Domains = []kinds.Domain{
		{Name: "main", Hosts: []string{"main.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}, Filters: toggle.Filters}}},
		{Name: "both", Hosts: []string{"both.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": toggle, "blog": toggle}},
	}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "yaoi")
	for i, it := range []struct {
		kind string
		tags []string
	}{{"gallery", []string{"yaoi"}}, {"gallery", nil}, {"blog", []string{"yaoi"}}, {"blog", nil}} {
		f.seed(item{n: i + 1, kind: it.kind, titles: map[string]string{"en": "Story"}, age: time.Hour, groups: it.tags, versions: []ver{{langs: []string{"en"}}}})
	}
	f.exec(`INSERT INTO {s}.content_filter_prefs(tenant_id,user_id,overrides) VALUES($1,'reader',$2)`, tenant, `{"`+f.ids["yaoi"]+`":"hide"}`)
	list := func(host, kind, user string, want ...int) {
		t.Helper()
		path := "/contents"
		if kind != "" {
			path += "?kind=" + kind
		}
		got, _ := f.list(host, user, path)
		expected := []string{}
		for _, n := range want {
			expected = append(expected, f.cid(n))
		}
		slices.Sort(got)
		slices.Sort(expected)
		if !slices.Equal(got, expected) {
			t.Fatalf("%s %s as %s: %v want %v", host, kind, user, got, expected)
		}
	}
	// A global hidden Yaoi preference cannot weaken required gallery policy;
	// the completely omitted blog policy stays unfiltered, even in mixed lists.
	list("main.test", "", "reader", 1, 3, 4)
	list("both.test", "gallery", "reader", 2)
	list("both.test", "blog", "reader", 4)
	save := func(kind string, choices []FilterSetting) {
		t.Helper()
		if status, _ := f.call(http.MethodPut, "both.test", "reader", "/me/content-filters", ContentFiltersUpdate{Kinds: map[string][]FilterSetting{kind: choices}}, nil); status != http.StatusOK {
			t.Fatal(status)
		}
	}
	save("gallery", []FilterSetting{{EntityID: f.ids["yaoi"], Visible: true}})
	list("both.test", "gallery", "reader", 1, 2)
	list("both.test", "blog", "reader", 4)
	save("blog", []FilterSetting{{EntityID: f.ids["yaoi"], Visible: true}})
	save("gallery", []FilterSetting{{EntityID: f.ids["yaoi"], Visible: false}})
	list("both.test", "gallery", "reader", 2)
	list("both.test", "blog", "reader", 3, 4)
	list("main.test", "", "reader", 1, 3, 4)
	save("gallery", nil)
	list("both.test", "gallery", "reader", 1, 2)
	list("both.test", "blog", "reader", 3, 4)
	values, _, err := f.svc.prefs(f.ctx, "reader")
	if err != nil || values[f.ids["yaoi"]] != hide {
		t.Fatalf("legacy data discarded: %v %v", values, err)
	}
	init := ContentFiltersInitialization{AccountID: "registered", Kinds: guestKinds(map[string][]FilterSetting{"gallery": {{EntityID: f.ids["yaoi"], Visible: true}}}), LegacyFilters: []FilterSetting{{EntityID: f.ids["yaoi"], Visible: false}}}
	if status, _ := f.call(http.MethodPost, "main.test", "registered", "/me/content-filters/initialize", init, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	list("both.test", "gallery", "registered", 1, 2)
	list("both.test", "blog", "registered", 4)
	list("main.test", "", "registered", 1, 3, 4)
}
