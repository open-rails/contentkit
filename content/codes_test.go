package content

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/open-rails/contentkit/access"
)

// Posts carry a content code (contenturl) from creation; edits re-slug it and
// never change the code.
func TestPostContentCode(t *testing.T) {
	rt, pool := newPostRuntime(t, Options{})
	h := postMux(rt)
	author := access.Actor{ID: "root1", Kind: "user"}
	rec := doJSON(t, h, author, "POST", "/posts", postWriteReq{Title: ptr("Hello, World!"), Body: ptr("b"), IsDraft: ptr(false)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	created := decodePost(t, rec)
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{9}$`).MatchString(created.Code) || created.URLSlug != "hello-world" {
		t.Fatalf("created post link %q %q", created.Code, created.URLSlug)
	}
	rec = doJSON(t, h, author, "PATCH", "/posts/"+created.ID, postWriteReq{Title: ptr("Second Title")})
	if v := decodePost(t, rec); v.Code != created.Code || v.URLSlug != "second-title" {
		t.Fatalf("renamed post link %q %q", v.Code, v.URLSlug)
	}
	rec = doJSON(t, h, author, "PATCH", "/posts/"+created.ID, postWriteReq{Slug: ptr("My Own Slug")})
	if v := decodePost(t, rec); v.Code != created.Code || v.URLSlug != "my-own-slug" {
		t.Fatalf("slugged post link %q %q", v.Code, v.URLSlug)
	}
	if got := listPosts(t, h, ""); len(got) != 1 || got[0].Code != created.Code || got[0].URLSlug != "my-own-slug" {
		t.Fatalf("listed %+v", got)
	}
	var kind, id string
	if err := pool.QueryRow(t.Context(), `SELECT content_kind, content_id FROM `+rt.store.t.codes+` WHERE tenant_id = $1 AND code = $2`, testTenant, created.Code).Scan(&kind, &id); err != nil || kind != KindPost || id != created.ID {
		t.Fatalf("registry row %q %q %v", kind, id, err)
	}
}
