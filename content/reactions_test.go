package content

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestReactions_TransitionsAndCounts(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("react: %v", err)
		}
	}
	must(reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(1)), 1))) // like
	assertCounts(t, rt, actor, ref("widget", cid(1)), 1, 0, 1)
	must(reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(1)), -1))) // switch to dislike
	assertCounts(t, rt, actor, ref("widget", cid(1)), 0, 1, -1)
	must(reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(1)), 0))) // neutral (not delete)
	assertCounts(t, rt, actor, ref("widget", cid(1)), 0, 0, 0)

	// second distinct user likes -> independent row
	must(reactErr(rt.reactions.react(ctx, access.Actor{ID: "u2", Kind: "user"}, ref("widget", cid(1)), 1)))
	assertCounts(t, rt, actor, ref("widget", cid(1)), 1, 0, 0)
}

func TestReactions_ConcurrentDoubleLikeIsExact(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(42), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	actor := access.Actor{ID: "racer", Kind: "user"}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- reactErr(rt.reactions.react(context.Background(), actor, rt.Ref("widget", cid(42)), 1))
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
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	if err := reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(901)), 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react on premium-locked: want ErrForbidden, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(902)), 1)); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("react on hidden: want ErrNotVisible, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(903)), 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("react on missing: want ErrNotFound, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, rt.Ref("unregistered", cid(1)), 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("react on unregistered kind: want ErrNotFound, got %v", err)
	}
}

func TestReactions_AnonymousDedupByIP(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	ctx := context.Background()
	anon := access.Actor{IP: "10.0.0.1", Anonymous: true}

	if err := reactErr(rt.reactions.react(ctx, anon, rt.Ref("widget", cid(1)), 1)); err != nil {
		t.Fatalf("anon like: %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, anon, rt.Ref("widget", cid(1)), 1)); err != nil {
		t.Fatalf("anon re-like: %v", err)
	}
	assertCounts(t, rt, anon, ref("widget", cid(1)), 1, 0, 1) // one like from the IP

	// unidentifiable actor (no id, no ip) is rejected
	if err := reactErr(rt.reactions.react(ctx, access.Actor{Anonymous: true}, rt.Ref("widget", cid(1)), 1)); err == nil {
		t.Fatal("expected rejection for unidentifiable actor")
	}
}

// A failing transaction leaves no reaction row behind.
func TestReactions_TransactionErrorRollsBack(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, pool := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE `+rt.store.t.counts); err != nil {
		t.Fatalf("drop counts table: %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, access.Actor{ID: "u1"}, rt.Ref("widget", cid(1)), 1)); err == nil {
		t.Fatal("react error = nil, want transaction failure")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.reactions).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reaction rows after rollback = %d err=%v, want 0", n, err)
	}
}

// An import recount waiting behind a live reaction counts that reaction once
// it commits, instead of writing the totals it saw before.
func TestReactions_ImportRecountCountsLiveReactionItWaitedOn(t *testing.T) {
	for _, kind := range []string{"gallery", KindComment} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			rt := moderatedRuntime(t, &fakeModerator{})
			target := rt.Ref("gallery", cid(1))
			if kind == KindComment {
				target = rt.Ref(KindComment, mustComment(t, rt, access.Actor{ID: "author"}, "gallery", cid(1), CommentInput{Body: "liked"}).ID)
			}
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			// The live like, as reactTx writes it, held open before commit.
			live, err := rt.store.beginMutation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer live.Rollback(context.Background())
			if kind == KindComment {
				if _, err := live.Exec(ctx, `SELECT 1 FROM `+rt.store.t.comments+` WHERE id=$1 FOR UPDATE`, target.ContentID); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := rt.reactions.applyTx(ctx, live, access.Actor{ID: "live"}, target.Key(), 1); err != nil {
				t.Fatal(err)
			}
			if kind == KindComment {
				if _, err := live.Exec(ctx, `UPDATE `+rt.store.t.comments+` SET likes = likes + 1 WHERE id=$1`, target.ContentID); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				tx, err := rt.store.beginMutation(ctx)
				if err == nil {
					defer tx.Rollback(context.Background())
					err = rt.ImportReactions(ctx, tx, []ImportReaction{{Ref: target, UserID: "imported", Value: 1}})
				}
				if err == nil {
					err = tx.Commit(ctx)
				}
				done <- err
			}()
			waitLockWaiters(t, ctx, rt, 1)
			if err := live.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := countsOf(t, rt, target); got.Likes != 2 {
				t.Fatalf("rollup likes=%d, want 2", got.Likes)
			}
			if kind == KindComment {
				var likes int
				if err := rt.store.pool.QueryRow(ctx, `SELECT likes FROM `+rt.store.t.comments+` WHERE id=$1`, target.ContentID).Scan(&likes); err != nil || likes != 2 {
					t.Fatalf("comment likes=%d err=%v, want 2", likes, err)
				}
			}
		})
	}
}

func TestReactions_HTTPRoute(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	h := rt.Handler()

	req := httptest.NewRequest("POST", "/contents/"+cid(1)+"/like", nil)
	req = testauth.As(req, access.Actor{ID: "u1", Kind: "user"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST like: status %d, body %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/contents/"+cid(903)+"/reaction", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown reaction: status %d, want 404", rec.Code)
	}
}

func assertCounts(t *testing.T, rt *Runtime, actor access.Actor, r contentref.ContentRef, wantLikes, wantDislikes int, wantMine int16) {
	t.Helper()
	c, err := rt.reactions.counts(context.Background(), rt.store.pool, actor, r.Key(), true)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if *c.Likes != wantLikes || *c.Dislikes != wantDislikes || c.Mine != wantMine {
		t.Fatalf("counts = %+v, want likes=%d dislikes=%d mine=%d", c, wantLikes, wantDislikes, wantMine)
	}
}
