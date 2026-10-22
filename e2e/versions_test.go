package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// versionsScenario reaches the versions module's routes, and media's
// version reads, over a premium book's version by owner with no media yet,
// created through the catalog's routes.
func versionsScenario(t *testing.T, w *world, f *server, staff, ownerID, owner, user, anon string) {
	creators := []catalog.Creator{{UserID: ownerID}}
	var made catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "book", Text: map[string]catalog.Text{"en": {Title: "Book"}},
		Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}, Creators: &creators,
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"en"}}}}, &made)
	f.want(http.StatusOK, staff, "POST", "/contents/"+made.ID+"/publish", catalog.PublishInput{}, &made)
	book, v1, v2 := made.ID, made.Versions[0].ID, made.Versions[1].ID
	missing := uuid.Must(uuid.NewV7()).String()
	// Each version has a code of its own, unique within the book: a version
	// read takes it, as does the codes route's ?v=; the page path stays the
	// language page.
	c1, c2 := made.Versions[0].Code, made.Versions[1].Code
	if _, err := contenturl.ParseVersionCode(c1); err != nil || c1 == c2 || c2 == "" {
		t.Fatalf("version codes %q %q", c1, c2)
	}
	var byCode catalog.Version
	f.want(200, anon, "GET", "/contents/"+book+"/versions/"+strings.ToLower(c2), nil, &byCode)
	if byCode.ID != v2 || byCode.Code != c2 {
		t.Fatalf("version by code %+v", byCode)
	}
	// Direct version lookup returns the parent with only the requested edition;
	// it does not inherit access from a different edition or expose a body.
	var selected catalog.Content
	f.want(http.StatusOK, anon, "GET", "/versions/"+v2, nil, &selected)
	if selected.ID != book || selected.SelectedVersionID != v2 || len(selected.Versions) != 1 || selected.Versions[0].ID != v2 || selected.Decision.Allowed || selected.Body != "" {
		t.Fatalf("direct version lookup changed selection or access: %+v", selected)
	}
	f.want(http.StatusNotFound, anon, "GET", "/versions/"+missing, nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/versions/"+book, nil, nil)
	f.want(404, anon, "GET", "/contents/"+book+"/versions/0000", nil, nil)
	var resolved contenturl.Resolved
	f.want(200, anon, "GET", "/codes/"+made.Code+"?v="+strings.ToLower(c2), nil, &resolved)
	if resolved.VersionID != v2 || resolved.Path != "/en/b/"+made.Code+"/book" {
		t.Fatalf("resolved with ?v= %+v", resolved)
	}
	resolved = contenturl.Resolved{}
	f.want(200, anon, "GET", "/codes/"+made.Code+"?lang=ja&v=nope", nil, &resolved)
	if resolved.VersionID != "" || resolved.Path != "/ja/b/"+made.Code+"/book" {
		t.Fatalf("an unknown ?v= names no version %+v", resolved)
	}
	path := "/contents/" + book + "/versions/" + v1
	var pending versions.Pending
	f.want(202, anon, "GET", path+"/reader", nil, &pending)
	f.want(404, anon, "GET", "/contents/"+missing+"/versions/"+missing+"/reader", nil, nil)
	f.want(401, anon, "PUT", path+"/reader", versions.ReaderMeta{SchemaVersion: 1}, nil)
	f.want(403, user, "PUT", path+"/reader", versions.ReaderMeta{SchemaVersion: 1}, nil)
	f.want(204, owner, "PUT", path+"/reader", versions.ReaderMeta{SchemaVersion: 1}, nil)
	f.want(403, user, "POST", path+"/fork", versions.ForkBody{From: v2}, nil)
	f.want(400, owner, "POST", path+"/fork", versions.ForkBody{}, nil)
	f.want(403, anon, "GET", path+"/download", nil, nil)
	f.want(409, owner, "GET", path+"/download", nil, nil) // no media yet: nothing to download
	// The published premium version shows, locked.
	var read media.ReadResult
	f.want(200, anon, "GET", "/media/book/"+book+"/versions/"+v1, nil, &read)
	if read.Access != media.AccessNone {
		t.Fatalf("a locked version read %+v", read)
	}
	f.want(404, anon, "GET", "/media/book/"+book+"/versions/"+v1+"/hls/hls/master.m3u8", nil, nil)
}
