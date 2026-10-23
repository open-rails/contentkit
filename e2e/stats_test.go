package e2e

import (
	"net/http"
	"testing"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
)

// Host sitemaps page the site's public content and group pages; staff read
// the site's statistics and readers their own, through AuthKit's gate.
func TestSitemapsAndStats(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	reader, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	g, _ := w.published(f, staff, "gallery")
	var made []entities.Entity
	f.want(http.StatusCreated, staff, "POST", "/entities", []entities.EntityInput{{Kind: "tag", Slug: "color",
		Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Color"}}}}, &made)
	f.want(http.StatusOK, staff, "PUT", "/contents/"+g+"/entities", entities.ContentEntities{Memberships: []entities.Membership{{EntityID: made[0].ID}}}, nil)
	f.want(http.StatusCreated, user, "POST", "/contents/"+g+"/comments", content.CommentInput{Body: "nice"}, nil)

	var page discovery.SitemapPage
	f.want(http.StatusOK, "", "GET", "/sitemap/contents?kind=gallery", nil, &page)
	if len(page.Entries) != 1 || page.Entries[0].ID != g || len(page.Entries[0].Languages) != 1 || page.Entries[0].Path == "" {
		t.Fatalf("content sitemap %+v", page)
	}
	f.want(http.StatusOK, "", "GET", "/sitemap/entities?kind=tag", nil, &page)
	if len(page.Entries) != 1 || page.Entries[0].ID != string(made[0].ID) {
		t.Fatalf("group sitemap %+v", page)
	}
	f.want(http.StatusBadRequest, "", "GET", "/sitemap/entities?kind=nope", nil, nil)

	f.want(http.StatusForbidden, user, "GET", "/stats/activity?kind=gallery", nil, nil)
	f.want(http.StatusNotImplemented, staff, "GET", "/stats/activity?kind=gallery", nil, nil) // no signal plane here
	var mine discovery.MyStats
	f.want(http.StatusOK, user, "GET", "/me/stats", nil, &mine)
	if mine.Comments != 1 || len(mine.Completed) != 0 {
		t.Fatalf("%s's stats %+v", reader.ID, mine)
	}
}
