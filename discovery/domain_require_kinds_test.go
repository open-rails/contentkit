package discovery

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
)

func TestDomainRequiredEntitiesCanBeLimitedToContentKinds(t *testing.T) {
	cfg := siteConfig()
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}
	required := []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}
	locked := false
	cfg.Domains = []kinds.Domain{
		{Name: "main", Hosts: []string{"main.test"}}, {Name: "scoped", Hosts: []string{"scoped.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: required}}}, {Name: "all", Hosts: []string{"all.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: required}, "video": {RequireGroups: required}, "blog": {RequireGroups: required}}}, {Name: "missing", Hosts: []string{"missing.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "missing"}}}}}, {Name: "missing-hard", Hosts: []string{"missing-hard.test"}, Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "missing"}, Visible: false, UserToggle: &locked}}, RequireGroups: required}, "video": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "missing"}, Visible: false, UserToggle: &locked}}}, "blog": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "missing"}, Visible: false, UserToggle: &locked}}}}},
	}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "yaoi")
	f.seed(item{n: 1, titles: map[string]string{"en": "Other gallery"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Yaoi gallery"}, age: time.Hour, groups: []string{"yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, kind: "blog", titles: map[string]string{"en": "News"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}})
	for _, tc := range []struct {
		host             string
		galleries, blogs int
	}{{"scoped.test", 1, 1}, {"all.test", 1, 0}, {"missing.test", 0, 1}, {"missing-hard.test", 0, 0}} {
		galleries, _ := f.list(tc.host, "", "/contents?kind=gallery")
		blogs, _ := f.list(tc.host, "", "/contents?kind=blog")
		if len(galleries) != tc.galleries || len(blogs) != tc.blogs {
			t.Fatalf("%s galleries %v blogs %v", tc.host, galleries, blogs)
		}
		r := httptest.NewRequest(http.MethodGet, "/contents/"+f.cid(3), nil)
		r.Host = tc.host
		w := httptest.NewRecorder()
		f.cat.Handler().ServeHTTP(w, r)
		expected := http.StatusOK
		if tc.blogs == 0 {
			expected = http.StatusNotFound
		}
		if w.Code != expected {
			t.Fatalf("direct blog %s: %d %s", tc.host, w.Code, w.Body)
		}
		var sitemap SitemapPage
		if status, _ := f.call(http.MethodGet, tc.host, "", "/sitemap/contents?kind=blog", nil, &sitemap); status != http.StatusOK || len(sitemap.Entries) != tc.blogs {
			t.Fatalf("blog sitemap %s: %d %+v", tc.host, status, sitemap)
		}
		domain := tc.host[:len(tc.host)-len(".test")]
		totals, err := f.cat.Totals(f.ctx, catalog.TotalsQuery{Domain: domain, Kinds: []string{"gallery", "blog"}})
		if err != nil || totals.Kinds["gallery"].Public != int64(tc.galleries) || totals.Kinds["blog"].Public != int64(tc.blogs) {
			t.Fatalf("totals %s: %+v %v", tc.host, totals, err)
		}
	}
}
