package contentkit

import (
	"context"
	"net/http"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
)

// A draft's code resolves only for an editor of it, privately and unindexed;
// anyone else gets the unknown code's 404 until it goes live.
func TestEngineCodePreviewIntegration(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	e, err := New(ctx, siteConfig(engineSchema(t, pool)), Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: ContentRoutes | CodeRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true}

	var c catalog.Content
	s.call(http.StatusCreated, staff, "POST", "/ck/v1/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Quiet Draft"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &c)
	code := "/ck/v1/codes/" + c.Code

	s.call(http.StatusNotFound, anon, "GET", code, nil, nil)
	s.call(http.StatusNotFound, user, "GET", code, nil, nil)
	var got contenturl.Resolved
	rec := s.call(http.StatusOK, staff, "GET", code, nil, &got)
	if got.ContentID != c.ID || got.Path == "" {
		t.Fatalf("preview %+v", got)
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("preview headers %v", rec.Header())
	}

	s.call(http.StatusOK, staff, "POST", "/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
	rec = s.call(http.StatusOK, anon, "GET", code, nil, &got)
	if rec.Header().Get("X-Robots-Tag") != "" {
		t.Fatalf("live content is indexable: %v", rec.Header())
	}
}
