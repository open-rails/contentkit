package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/access"
)

type pausedScreen struct{ entered, release chan struct{} }

func (*pausedScreen) StatelessPolicy() {}
func (p *pausedScreen) Screen(_ context.Context, in ModerationInput) (Verdict, error) {
	if in.Text == "paused edit" {
		close(p.entered)
		<-p.release
	}
	return Verdict{Decision: DecisionApprove}, nil
}
func oneConnectionPostRuntime(t *testing.T, mod ContentModerator) *Runtime {
	t.Helper()
	rt, _ := newPostRuntime(t, Options{Moderator: mod})
	cfg := rt.store.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	out, err := New(context.Background(), Options{Pool: pool, Schema: rt.schema, Tenant: rt.tenant, Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{}, Moderator: mod, Perms: Perms{PostWrite: postPerm}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func patchPostWithContext(rt *Runtime, ctx context.Context, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PATCH", "/posts/"+id, strings.NewReader(`{"body":"`+body+`"}`))
	req = req.WithContext(withActor(ctx, access.Actor{ID: "author"}))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}
func TestPostScreeningNeedsOnlyOneConnection(t *testing.T) {
	rt := oneConnectionPostRuntime(t, &fakeModerator{})
	id := insertPost(t, rt)
	if _, err := rt.store.pool.Exec(context.Background(), `UPDATE `+rt.store.t.posts+` SET is_draft=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := patchPostWithContext(rt, ctx, id, "fine edit")
	if rec.Code != http.StatusOK {
		t.Fatalf("single connection edit blocked: %d %s", rec.Code, rec.Body.String())
	}
}
func TestPausedPostScreeningDoesNotBlockErasure(t *testing.T) {
	mod := &pausedScreen{make(chan struct{}), make(chan struct{})}
	rt := oneConnectionPostRuntime(t, mod)
	rec := doJSON(t, rt.Handler(), access.Actor{ID: "author"}, "POST", "/posts", postWriteReq{Title: ptr("title"), Body: ptr("public"), IsDraft: ptr(false)})
	post := decodePost(t, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- patchPostWithContext(rt, ctx, post.ID, "paused edit") }()
	<-mod.entered
	err := rt.EraseSubjects(ctx, []string{"author"})
	close(mod.release)
	late := <-done
	if err != nil {
		t.Fatalf("blocked provider held source connection/lock: %v", err)
	}
	if late.Code != http.StatusForbidden {
		t.Fatalf("erased source accepted delayed edit: %d %s", late.Code, late.Body.String())
	}
}
func TestPausedPostScreeningCannotOverwriteNewerEdit(t *testing.T) {
	mod := &pausedScreen{make(chan struct{}), make(chan struct{})}
	rt := oneConnectionPostRuntime(t, mod)
	id := insertPost(t, rt)
	if _, err := rt.store.pool.Exec(context.Background(), `UPDATE `+rt.store.t.posts+` SET is_draft=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- patchPostWithContext(rt, ctx, id, "paused edit") }()
	<-mod.entered
	newer := patchPostWithContext(rt, ctx, id, "newer edit")
	close(mod.release)
	older := <-done
	if newer.Code != http.StatusOK || older.Code != http.StatusConflict {
		t.Fatalf("new=%d old=%d %s", newer.Code, older.Code, older.Body.String())
	}
	var body string
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.posts+` WHERE id=$1`, id).Scan(&body); err != nil || body != "newer edit" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}
func TestPausedCommentScreeningCannotOverwriteNewerEdit(t *testing.T) {
	mod := &pausedScreen{make(chan struct{}), make(chan struct{})}
	rt := moderatedRuntime(t, mod)
	author := access.Actor{ID: "author"}
	ctx := context.Background()
	cm := mustComment(t, rt, author, "gallery", "1", createInput{Body: "public"})
	done := make(chan error, 1)
	go func() { _, err := rt.comments.edit(ctx, author, cm.ID, "paused edit"); done <- err }()
	<-mod.entered
	_, err := rt.comments.edit(ctx, author, cm.ID, "newer edit")
	close(mod.release)
	older := <-done
	if err != nil || older != errContentChanged {
		t.Fatalf("new=%v old=%v", err, older)
	}
}
