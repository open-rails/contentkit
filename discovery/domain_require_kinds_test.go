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
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Entities: []string{"tag"}, Versions: kinds.VersionRules{Languages: "one"}}
	required := []kinds.EntityRef{{Kind: "tag", Slug: "yaoi"}}
	locked := false
	cfg.ContentFilters.Domains, cfg.ContentFilters.AppliesTo = []kinds.Domain{
		{Name: "main", Hosts: []string{"main.test"}}, {Name: "scoped", Hosts: []string{"scoped.test"}, RequireEntities: map[string][]kinds.EntityRef{"gallery": required}}, {Name: "all", Hosts: []string{"all.test"}, RequireEntities: map[string][]kinds.EntityRef{"gallery": required, "video": required, "blog": required}}, {Name: "missing", Hosts: []string{"missing.test"}, RequireEntities: map[string][]kinds.EntityRef{"gallery": []kinds.EntityRef{{Kind: "tag", Slug: "missing"}}}}, {Name: "missing-hard", Hosts: []string{"missing-hard.test"}, RequireEntities: map[string][]kinds.EntityRef{"gallery": required}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "missing"}, Visible: false, UserToggle: &locked}}},
	}, []string{"gallery", "video", "blog"}
	f := newFixtureConfig(t, nil, "", cfg)
	f.entity("tag", "yaoi")
	f.seed(item{n: 1, titles: map[string]string{"en": "Other gallery"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Yaoi gallery"}, age: time.Hour, entities: []string{"yaoi"}, versions: []ver{{langs: []string{"en"}}}},
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
