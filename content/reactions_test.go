package content

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

func TestReactions_TransitionsAndCounts(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("react: %v", err)
		}
	}
	must(reactErr(rt.reactions.react(ctx, actor, "widget", cid(1), 1))) // like
	assertCounts(t, rt, actor, ref("widget", cid(1)), 1, 0, 1)
	must(reactErr(rt.reactions.react(ctx, actor, "widget", cid(1), -1))) // switch to dislike
	assertCounts(t, rt, actor, ref("widget", cid(1)), 0, 1, -1)
	must(reactErr(rt.reactions.react(ctx, actor, "widget", cid(1), 0))) // neutral (not delete)
	assertCounts(t, rt, actor, ref("widget", cid(1)), 0, 0, 0)

	// second distinct user likes -> independent row
	must(reactErr(rt.reactions.react(ctx, access.Actor{ID: "u2", Kind: "user"}, "widget", cid(1), 1)))
	assertCounts(t, rt, actor, ref("widget", cid(1)), 1, 0, 0)
}

func TestReactions_ConcurrentDoubleLikeIsExact(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(42), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	actor := access.Actor{ID: "racer", Kind: "user"}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- reactErr(rt.reactions.react(context.Background(), actor, "widget", cid(42), 1))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent react: %v", err)
		}
	}
	// 20 concurrent identical likes from one actor => exactly one like.
	assertCounts(t, rt, actor, ref("widget", cid(42)), 1, 0, 1)
}

func TestReactions_GatingRejectsInaccessibleAndMissing(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(901), true, false)  // visible but premium-locked
	res.set("widget", cid(902), false, false) // unpublished/deleted
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	if err := reactErr(rt.reactions.react(ctx, actor, "widget", cid(901), 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react on premium-locked: want ErrForbidden, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "widget", cid(902), 1)); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("react on hidden: want ErrNotVisible, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "widget", cid(903), 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("react on missing: want ErrNotFound, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "unregistered", cid(1), 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("react on unregistered kind: want ErrNotFound, got %v", err)
	}
}

func TestReactions_AnonymousDedupByIP(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	ctx := context.Background()
	anon := access.Actor{IP: "10.0.0.1", Anonymous: true}

	if err := reactErr(rt.reactions.react(ctx, anon, "widget", cid(1), 1)); err != nil {
		t.Fatalf("anon like: %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, anon, "widget", cid(1), 1)); err != nil {
		t.Fatalf("anon re-like: %v", err)
	}
	assertCounts(t, rt, anon, ref("widget", cid(1)), 1, 0, 1) // one like from the IP

	// unidentifiable actor (no id, no ip) is rejected
	if err := reactErr(rt.reactions.react(ctx, access.Actor{Anonymous: true}, "widget", cid(1), 1)); err == nil {
		t.Fatal("expected rejection for unidentifiable actor")
	}
}

// A failing transaction leaves no reaction row behind.
func TestReactions_TransactionErrorRollsBack(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, pool := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE `+rt.store.t.counts); err != nil {
		t.Fatalf("drop counts table: %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, access.Actor{ID: "u1"}, "widget", cid(1), 1)); err == nil {
		t.Fatal("react error = nil, want transaction failure")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.reactions).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reaction rows after rollback = %d err=%v, want 0", n, err)
	}
}

func TestReactions_HTTPRoute(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	h := rt.Handler()

	req := httptest.NewRequest("POST", "/widget/"+cid(1)+"/like", nil)
	req = req.WithContext(withActor(req.Context(), access.Actor{ID: "u1", Kind: "user"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST like: status %d, body %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/widget/"+cid(903)+"/reaction", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown reaction: status %d, want 404", rec.Code)
	}
}

func assertCounts(t *testing.T, rt *Runtime, actor access.Actor, r contentref.ContentRef, wantLikes, wantDislikes int, wantMine int16) {
	t.Helper()
	c, err := rt.reactions.counts(context.Background(), rt.store.pool, actor, r.Key())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.Likes != wantLikes || c.Dislikes != wantDislikes || c.Mine != wantMine {
		t.Fatalf("counts = %+v, want likes=%d dislikes=%d mine=%d", c, wantLikes, wantDislikes, wantMine)
	}
}
