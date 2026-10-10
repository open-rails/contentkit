package content

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contenturl"
)

// Staff find posts by text, list the deleted ones on their own and restore
// them as they were: public again, media exposed, slug checked.
func TestPostRestoreAndStaffSearch(t *testing.T) {
	ctx := context.Background()
	m := &testMedia{}
	rt, _ := newPostRuntime(t, Options{Authz: postRoleAuthz{writers: map[string]bool{"admin": true}}, Media: m.options()})
	h := rt.Handler()
	admin, reader := access.Actor{ID: "admin", Kind: "user"}, access.Actor{ID: "reader", Kind: "user"}
	create := func(in PostInput) Post {
		t.Helper()
		rec := doJSON(t, h, admin, "POST", "/posts", in)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
		return decodePost(t, rec)
	}
	festival := create(PostInput{Title: ptr("Summer Festival"), Body: ptr("fireworks at nine"), Excerpt: ptr("A night out")})
	notes := create(PostInput{Title: ptr("Winter notes"), Body: ptr("100% snow"), Slug: ptr("notes")})
	list := func(query string) []string {
		t.Helper()
		rec := doJSON(t, h, admin, "GET", "/posts/admin"+query, nil)
		var posts []Post
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &posts) != nil {
			t.Fatalf("GET /posts/admin%s: %d %s", query, rec.Code, rec.Body)
		}
		ids := make([]string, len(posts))
		for i, p := range posts {
			ids[i] = p.ID
			if (p.DeletedAt != nil) != (query == "?deleted=true") {
				t.Fatalf("GET /posts/admin%s: deleted_at %v", query, p.DeletedAt)
			}
		}
		return ids
	}
	for query, want := range map[string][]string{
		"?q=FESTIVAL": {festival.ID}, "?q=fireworks": {festival.ID}, "?q=night+out": {festival.ID},
		"?q=notes": {notes.ID}, "?q=0%25+snow": {notes.ID}, "?q=%25": {notes.ID}, "?q=_": {}, "?q=+": {notes.ID, festival.ID},
	} {
		if got := list(query); !slices.Equal(got, want) {
			t.Errorf("GET /posts/admin%s = %v, want %v", query, got, want)
		}
	}

	if rec := doJSON(t, h, admin, "DELETE", "/posts/"+festival.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if got := list("?deleted=true"); !slices.Equal(got, []string{festival.ID}) {
		t.Fatalf("deleted list %v", got)
	}
	if got := list("?deleted=true&q=winter"); len(got) != 0 {
		t.Fatalf("deleted list searched %v", got)
	}
	if got := list("?deleted=false"); !slices.Equal(got, []string{notes.ID}) {
		t.Fatalf("live list %v", got)
	}
	if rec := doJSON(t, h, admin, "GET", "/posts/admin?deleted=maybe", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("deleted=maybe: %d", rec.Code)
	}
	if vis, err := rt.PostVisibility(ctx, festival.ID); err != nil || vis != contenturl.Gone {
		t.Fatalf("deleted visibility %v %v", vis, err)
	}

	exposed := len(m.exposed)
	if rec := doJSON(t, h, reader, "POST", "/posts/"+festival.ID+"/restore", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("restore without PostWrite: %d", rec.Code)
	}
	rec := doJSON(t, h, admin, "POST", "/posts/"+festival.ID+"/restore", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if got := decodePost(t, rec); got.Title != festival.Title || got.Body != festival.Body || got.DeletedAt != nil || got.Code != festival.Code {
		t.Fatalf("restored %+v", got)
	}
	if len(m.exposed) != exposed+1 {
		t.Fatalf("restore exposed %v", m.exposed[exposed:])
	}
	if rec := doJSON(t, h, reader, "GET", "/posts/"+festival.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("restored post for a reader: %d", rec.Code)
	}
	if vis, err := rt.PostVisibility(ctx, festival.ID); err != nil || vis != contenturl.Visible {
		t.Fatalf("restored visibility %v %v", vis, err)
	}
	for _, id := range []string{festival.ID, "missing"} {
		if rec := doJSON(t, h, admin, "POST", "/posts/"+id+"/restore", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("restore %s: %d", id, rec.Code)
		}
	}

	// A live post took the slug meanwhile: restoring refuses, and changes nothing.
	if rec := doJSON(t, h, admin, "DELETE", "/posts/"+notes.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete notes: %d", rec.Code)
	}
	create(PostInput{Title: ptr("New notes"), Body: ptr("b"), Slug: ptr("notes")})
	rec = doJSON(t, h, admin, "POST", "/posts/"+notes.ID+"/restore", nil)
	if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != CodeConflict {
		t.Fatalf("restore over a taken slug: %d %s", rec.Code, rec.Body)
	}
	if got := list("?deleted=true"); !slices.Equal(got, []string{notes.ID}) {
		t.Fatalf("deleted list after the refusal %v", got)
	}
}
