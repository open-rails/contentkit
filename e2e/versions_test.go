package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
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
	f.want(404, owner, "GET", path+"/download", nil, nil)
	// The published premium version shows, locked.
	var read media.ReadResult
	f.want(200, anon, "GET", "/media/book/"+book+"/versions/"+v1, nil, &read)
	if read.Access != media.AccessNone {
		t.Fatalf("a locked version read %+v", read)
	}
	f.want(404, anon, "GET", "/media/book/"+book+"/versions/"+v1+"/hls/hls/master.m3u8", nil, nil)
}
