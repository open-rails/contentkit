package discovery

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/testauth"
)

// A video lists once it plays: browsing, search by ids and sitemaps leave
// out one whose media does not play yet, while the reader's favorites (and
// their summary), known cards and staff still reach it. A version whose fact
// is unknown, from before media recorded it, lists.
func TestUnplayableVideoStaysOutOfBrowsing(t *testing.T) {
	f := newFixture(t, nil, "")
	f.seed(
		item{n: 1, kind: "video", titles: map[string]string{"en": "Ready"}, age: 3 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, kind: "video", titles: map[string]string{"en": "Encoding"}, age: 2 * time.Hour, versions: []ver{{langs: []string{"en"}, unplayable: true}}},
		item{n: 3, kind: "video", titles: map[string]string{"en": "Older"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
	)
	f.exec(`UPDATE {s}.content_versions SET playable = NULL WHERE content_id = $1`, f.cid(3))
	if err := f.cat.RefreshFacets(f.ctx, f.pool, []string{f.cid(3)}); err != nil {
		t.Fatal(err)
	}
	const host = "main.test"
	browse := func(user string) []string {
		t.Helper()
		got, _ := f.list(host, user, q("/contents", "kind", "video"))
		return got
	}
	if got := browse(""); !reflect.DeepEqual(got, []string{f.cid(3), f.cid(1)}) {
		t.Fatalf("browsing = %v, want the playable and the unknown video", got)
	}
	if got, _ := f.list(host, "", q("/contents", "ids", f.cid(2))); len(got) != 0 {
		t.Fatalf("a public read by ids = %v, want nothing", got)
	}
	var sitemap SitemapPage
	if status, code := f.call(http.MethodGet, host, "", q("/sitemap/contents", "kind", "video"), nil, &sitemap); status != http.StatusOK {
		t.Fatalf("sitemap: %d %s", status, code)
	}
	if len(sitemap.Entries) != 2 {
		t.Fatalf("sitemap = %+v, want the playable and the unknown video", sitemap.Entries)
	}
	var known ContentPage
	if status, code := f.call(http.MethodGet, host, "", q("/content-cards", "ids", strings.Join([]string{f.cid(2)}, ","), "summary", "true"), nil, &known); status != http.StatusOK {
		t.Fatalf("known cards: %d %s", status, code)
	}
	if len(known.Contents) != 1 || known.Contents[0].ID != f.cid(2) || known.Summary == nil || known.Summary.Total != 1 {
		t.Fatalf("known cards = %+v (summary %+v), want the unplayable video", known.Contents, known.Summary)
	}
	f.favorite(1, "reader", f.now.Add(-time.Minute))
	f.favorite(2, "reader", f.now)
	var favorites InteractionPage
	if status, code := f.call(http.MethodGet, host, "reader", q("/me/favorites", "kind", "video", "summary", "true"), nil, &favorites); status != http.StatusOK {
		t.Fatalf("favorites: %d %s", status, code)
	}
	if len(favorites.Items) != 2 || favorites.Items[0].Content == nil || favorites.Items[0].Content.ID != f.cid(2) {
		t.Fatalf("favorites = %+v, want both videos", favorites.Items)
	}
	if favorites.Summary == nil || favorites.Summary.Total != 2 {
		t.Fatalf("favorites summary = %+v, want a total of 2", favorites.Summary)
	}
	if got := browse("staff"); len(got) != 3 {
		t.Fatalf("staff browsing = %v, want every video", got)
	}

	// Its first rendition lands: media records it and the facets follow.
	f.exec(`UPDATE {s}.content_versions SET playable = true WHERE content_id = $1`, f.cid(2))
	if err := f.cat.RefreshFacets(f.ctx, f.pool, []string{f.cid(2)}); err != nil {
		t.Fatal(err)
	}
	if got := browse(""); !reflect.DeepEqual(got, []string{f.cid(3), f.cid(2), f.cid(1)}) {
		t.Fatalf("browsing after it plays = %v", got)
	}
}

// A new video version starts not playable until its media says otherwise;
// a gallery's fact stays unknown.
func TestNewVideoVersionStartsUnplayable(t *testing.T) {
	f := newFixture(t, nil, "")
	f.seed(
		item{n: 1, kind: "video", titles: map[string]string{"en": "New"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Gallery"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
	)
	staff := testauth.Actor(access.Actor{ID: "staff"}, staffOnly{})
	if _, err := f.cat.PutVersions(f.ctx, staff, []catalog.VersionPut{{ContentID: f.cid(1), VersionInput: catalog.VersionInput{Languages: []string{"ja"}}}}); err != nil {
		t.Fatal(err)
	}
	var video, gallery *bool
	if err := f.pool.QueryRow(f.ctx, `SELECT playable FROM `+f.table("content_versions")+` WHERE content_id = $1 AND 'ja' = ANY(languages)`, f.cid(1)).Scan(&video); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT playable FROM `+f.table("content_versions")+` WHERE content_id = $1`, f.cid(2)).Scan(&gallery); err != nil {
		t.Fatal(err)
	}
	if video == nil || *video || gallery != nil {
		t.Fatalf("new video version playable = %v, gallery = %v; want false and unknown", video, gallery)
	}
}
