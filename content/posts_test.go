package content

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
)

// postPerm is the opaque PostWrite permission wired for the posts tests.
const postPerm = "post:write"

// newPostRuntime builds a Runtime with PostWrite set (an unset perm fails closed,
// so allowAll alone would still 403). Mirrors newTestRuntime otherwise.
func newPostRuntime(t *testing.T, opts Options) (*Runtime, *pgxpool.Pool) {
	t.Helper()
	if opts.Perms.PostWrite == "" {
		opts.Perms.PostWrite = postPerm
	}
	return newTestRuntime(t, opts)
}

// postMux mounts only the posts routes.
func postMux(rt *Runtime) http.Handler {
	mux := http.NewServeMux()
	newPosts(rt).mount(mux)
	return mux
}

// postErrAuthz denies by erroring — proves fail-closed (error must not allow).
type postErrAuthz struct{}

func (postErrAuthz) Can(context.Context, Actor, string) (bool, error) {
	return false, fmt.Errorf("authz backend down")
}

// postRoleAuthz grants PostWrite only to actors in writers — one runtime can then
// serve both a privileged and an unprivileged caller.
type postRoleAuthz struct{ writers map[string]bool }

func (a postRoleAuthz) Can(_ context.Context, actor Actor, _ string) (bool, error) {
	return a.writers[actor.ID], nil
}

type processorFunc func(context.Context, string) (string, error)

func (f processorFunc) Sanitize(ctx context.Context, raw string) (string, error) {
	return f(ctx, raw)
}

// doJSON issues a request with an actor on context and returns the recorder.
func doJSON(t *testing.T, h http.Handler, actor Actor, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	req = req.WithContext(withActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodePost(t *testing.T, rec *httptest.ResponseRecorder) postView {
	t.Helper()
	var v postView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode post: %v (body=%s)", err, rec.Body.String())
	}
	return v
}

func ptr[T any](v T) *T { return &v }

func TestPostCRUDHappyPath(t *testing.T) {
	rt, _ := newPostRuntime(t, Options{})
	h := postMux(rt)
	author := Actor{ID: "root1", Kind: "user"}

	// create (published so it lands in the public list)
	rec := doJSON(t, h, author, "POST", "/posts", postWriteReq{
		Title: ptr("Hello"), Body: ptr("<b>world</b>"), IsDraft: ptr(false),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	created := decodePost(t, rec)
	if created.ID == "" || created.AuthorID != "root1" {
		t.Fatalf("create returned %+v", created)
	}

	// get
	rec = doJSON(t, h, author, "GET", "/posts/"+created.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d body %s", rec.Code, rec.Body.String())
	}

	// update
	rec = doJSON(t, h, author, "PATCH", "/posts/"+created.ID, postWriteReq{Title: ptr("Hello (edited)")})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d body %s", rec.Code, rec.Body.String())
	}
	if got := decodePost(t, rec).Title; got != "Hello (edited)" {
		t.Fatalf("update title = %q", got)
	}

	// present in list before delete
	if n := len(listPosts(t, h, "")); n != 1 {
		t.Fatalf("list before delete = %d, want 1", n)
	}

	// soft delete
	rec = doJSON(t, h, author, "DELETE", "/posts/"+created.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body.String())
	}

	// gone from list and from get
	if n := len(listPosts(t, h, "")); n != 0 {
		t.Fatalf("list after delete = %d, want 0", n)
	}
	if rec = doJSON(t, h, author, "GET", "/posts/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, want 404", rec.Code)
	}
	// double delete is 404
	if rec = doJSON(t, h, author, "DELETE", "/posts/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete: status %d, want 404", rec.Code)
	}
}

func TestPostBodyProcessorIsIndependent(t *testing.T) {
	plain := processorFunc(func(_ context.Context, raw string) (string, error) {
		return "plain:" + raw, nil
	})
	rich := processorFunc(func(_ context.Context, raw string) (string, error) {
		return "rich:" + raw, nil
	})
	rt, _ := newPostRuntime(t, Options{Processor: plain, PostBodyProcessor: rich})
	rec := doJSON(t, postMux(rt), Actor{ID: "root1"}, "POST", "/posts", postWriteReq{
		Title:   ptr("processors"),
		Body:    ptr("body"),
		Excerpt: ptr("excerpt"),
		IsDraft: ptr(false),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	post := decodePost(t, rec)
	if post.Body != "rich:body" {
		t.Fatalf("body = %q, want rich processor output", post.Body)
	}
	if post.Excerpt == nil || *post.Excerpt != "plain:excerpt" {
		t.Fatalf("excerpt = %v, want plain processor output", post.Excerpt)
	}
}

func TestPostPermissionGate(t *testing.T) {
	author := Actor{ID: "root1", Kind: "user"}
	body := postWriteReq{Title: ptr("x"), Body: ptr("y")}

	// denyAll: every write is 403.
	rtDeny, _ := newPostRuntime(t, Options{Authz: denyAll{}})
	hDeny := postMux(rtDeny)
	if rec := doJSON(t, hDeny, author, "POST", "/posts", body); rec.Code != http.StatusForbidden {
		t.Fatalf("create under denyAll: status %d, want 403", rec.Code)
	}
	if rec := doJSON(t, hDeny, author, "PATCH", "/posts/whatever", body); rec.Code != http.StatusForbidden {
		t.Fatalf("update under denyAll: status %d, want 403", rec.Code)
	}
	if rec := doJSON(t, hDeny, author, "DELETE", "/posts/whatever", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("delete under denyAll: status %d, want 403", rec.Code)
	}

	// FAIL-CLOSED: an authz error must NOT allow the write.
	rtErr, _ := newPostRuntime(t, Options{Authz: postErrAuthz{}})
	if rec := doJSON(t, postMux(rtErr), author, "POST", "/posts", body); rec.Code != http.StatusForbidden {
		t.Fatalf("create under authz error: status %d, want 403 (fail-closed)", rec.Code)
	}
}

func TestPostDraftVisibility(t *testing.T) {
	// One runtime; "editor" holds PostWrite, "reader" does not.
	authz := postRoleAuthz{writers: map[string]bool{"editor": true}}
	rt, _ := newPostRuntime(t, Options{Authz: authz})
	h := postMux(rt)
	editor := Actor{ID: "editor", Kind: "user"}
	reader := Actor{ID: "reader", Kind: "user"}

	rec := doJSON(t, h, editor, "POST", "/posts", postWriteReq{
		Title: ptr("secret"), Body: ptr("draft body"), IsDraft: ptr(true),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: status %d body %s", rec.Code, rec.Body.String())
	}
	draft := decodePost(t, rec)

	// not in the public list
	if n := len(listPosts(t, h, "")); n != 0 {
		t.Fatalf("draft in public list (%d), want 0", n)
	}
	// hidden (404) from a caller without PostWrite
	if rec = doJSON(t, h, reader, "GET", "/posts/"+draft.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("draft get by reader: status %d, want 404", rec.Code)
	}
	// visible (200) to a PostWrite holder
	if rec = doJSON(t, h, editor, "GET", "/posts/"+draft.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("draft get by editor: status %d, want 200", rec.Code)
	}
}

func TestPostListSortedAndCounts(t *testing.T) {
	rt, _ := newPostRuntime(t, Options{})
	h := postMux(rt)
	author := Actor{ID: "root1"}

	// Two published posts with explicit past live_at so order is deterministic.
	older := postWriteReq{Title: ptr("older"), Body: ptr("b"), IsDraft: ptr(false), LiveAt: ptr(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))}
	newer := postWriteReq{Title: ptr("newer"), Body: ptr("b"), IsDraft: ptr(false), LiveAt: ptr(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))}
	if rec := doJSON(t, h, author, "POST", "/posts", older); rec.Code != http.StatusCreated {
		t.Fatalf("create older: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, author, "POST", "/posts", newer); rec.Code != http.StatusCreated {
		t.Fatalf("create newer: %d %s", rec.Code, rec.Body.String())
	}

	list := listPosts(t, h, "")
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].Title != "newer" || list[1].Title != "older" {
		t.Fatalf("list order = [%q, %q], want newest-first", list[0].Title, list[1].Title)
	}
	// no comments inserted -> comment_count is computed as 0
	if list[0].CommentCount != 0 {
		t.Fatalf("comment_count = %d, want 0", list[0].CommentCount)
	}
}

func TestPostLikeBumpsCountersConcurrentExact(t *testing.T) {
	rt, pool := newPostRuntime(t, Options{})
	ctx := context.Background()

	// Insert a published post directly (react has no perm gate; it needs a target).
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO `+rt.store.t.posts+`
		(tenant_id, author_id, title, body, is_draft) VALUES ($1, 'a', 't', 'b', false) RETURNING id`, testTenant).Scan(&id); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	p := newPosts(rt)
	actor := Actor{ID: "racer", Kind: "user"}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.react(context.Background(), actor, id, 1)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent like: %v", err)
		}
	}

	// 20 concurrent identical likes from one actor => exactly one like.
	v, err := p.loadByID(ctx, pool, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if v.TotalLikes != 1 || v.TotalDislikes != 0 {
		t.Fatalf("counters = (%d,%d), want (1,0)", v.TotalLikes, v.TotalDislikes)
	}

	// switch to dislike -> split counters move exactly
	if err := p.react(ctx, actor, id, -1); err != nil {
		t.Fatalf("switch to dislike: %v", err)
	}
	if v, _ = p.loadByID(ctx, pool, id); v.TotalLikes != 0 || v.TotalDislikes != 1 {
		t.Fatalf("after switch = (%d,%d), want (0,1)", v.TotalLikes, v.TotalDislikes)
	}

	// reacting on a draft/absent target is 404 (ErrNotFound)
	var draftID string
	if err := pool.QueryRow(ctx, `INSERT INTO `+rt.store.t.posts+`
		(tenant_id, author_id, title, body, is_draft) VALUES ($1, 'a', 't', 'b', true) RETURNING id`, testTenant).Scan(&draftID); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	if err := p.react(ctx, actor, draftID, 1); err == nil {
		t.Fatal("expected react on draft to fail")
	}
}

func TestPostLikeHTTPRoute(t *testing.T) {
	rt, _ := newPostRuntime(t, Options{})
	// Mount BOTH modules on one mux to prove /posts/{id}/like doesn't collide
	// with reactions' /{type}/{id}/like.
	mux := http.NewServeMux()
	rt.reactions.mount(mux)
	newPosts(rt).mount(mux)

	author := Actor{ID: "root1"}
	rec := doJSON(t, mux, author, "POST", "/posts", postWriteReq{Title: ptr("t"), Body: ptr("b"), IsDraft: ptr(false)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	id := decodePost(t, rec).ID

	rec = doJSON(t, mux, author, "POST", "/posts/"+id+"/like", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("like: status %d body %s", rec.Code, rec.Body.String())
	}
	if got := decodePost(t, rec).TotalLikes; got != 1 {
		t.Fatalf("total_likes = %d, want 1", got)
	}
}

// listPosts fetches GET /posts (optionally filtered by language) and decodes it.
func listPosts(t *testing.T, h http.Handler, language string) []postView {
	t.Helper()
	target := "/posts"
	if language != "" {
		target += "?language=" + language
	}
	rec := doJSON(t, h, Actor{Anonymous: true}, "GET", target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	var out []postView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// Every post write queues the post as a keyword document in the search
// schema's dirty queue inside the same transaction; the module's builder
// returns a document only for a published post in that language.
func TestPostWritesQueueKeywordDocuments(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	searchSchema := pgtest.Schema(t, ctx, pool)
	rt, _ := newPostRuntime(t, Options{Schema: searchSchema})
	h := postMux(rt)
	author := Actor{ID: "root1", Kind: "user"}

	rec := doJSON(t, h, author, "POST", "/posts", postWriteReq{Title: ptr("Hello"), Body: ptr("b"), Language: ptr("en"), Slug: ptr("hello"), IsDraft: ptr(false)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := decodePost(t, rec).ID
	dirty := func() map[string]bool {
		rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT language, is_deleted FROM %s.content_search_dirty WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3 AND reason = 'post'`, searchSchema), testTenant, KindPost, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var lang string
			var deleted bool
			if err := rows.Scan(&lang, &deleted); err != nil {
				t.Fatal(err)
			}
			out[lang] = deleted
		}
		return out
	}
	if got := dirty(); len(got) != 1 || got["en"] {
		t.Fatalf("dirty after create = %v, want en rebuild", got)
	}
	docs, err := rt.KeywordDocuments(ctx, testTenant, KindPost, "en", []contentref.ContentRef{rt.Ref(KindPost, id)})
	if err != nil || len(docs) != 1 || docs[0].Title != "Hello" || docs[0].Aliases[0] != "hello" || docs[0].Language != "en" {
		t.Fatalf("KeywordDocuments = %+v err=%v", docs, err)
	}
	refs, _, done, err := rt.ListContent(ctx, testTenant, KindPost, "en", "", 10)
	if err != nil || !done || len(refs) != 1 || refs[0].ContentID != id {
		t.Fatalf("ListContent = %v done=%v err=%v", refs, done, err)
	}

	// A language change deletes the old-language document and queues the new one.
	if rec = doJSON(t, h, author, "PATCH", "/posts/"+id, postWriteReq{Language: ptr("ja")}); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if got := dirty(); !got["en"] || got["ja"] {
		t.Fatalf("dirty after language change = %v, want en deleted and ja rebuild", got)
	}
	if docs, _ := rt.KeywordDocuments(ctx, testTenant, KindPost, "en", []contentref.ContentRef{rt.Ref(KindPost, id)}); len(docs) != 0 {
		t.Fatalf("an en document survives the language change: %+v", docs)
	}
	// Unpublishing yields no document; deleting queues a deletion.
	if rec = doJSON(t, h, author, "PATCH", "/posts/"+id, postWriteReq{IsDraft: ptr(true)}); rec.Code != http.StatusOK {
		t.Fatalf("draft: %d", rec.Code)
	}
	if docs, _ := rt.KeywordDocuments(ctx, testTenant, KindPost, "ja", []contentref.ContentRef{rt.Ref(KindPost, id)}); len(docs) != 0 {
		t.Fatalf("a draft yields a document: %+v", docs)
	}
	if rec = doJSON(t, h, author, "DELETE", "/posts/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if got := dirty(); !got["ja"] {
		t.Fatalf("dirty after delete = %v, want ja deleted", got)
	}
	// Another tenant's post is neither built nor listed by this runtime.
	if _, err := rt.KeywordDocuments(ctx, "other", KindPost, "en", nil); err == nil {
		t.Fatal("KeywordDocuments accepted another tenant")
	}
}
