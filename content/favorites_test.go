package content

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestFavorites_AddRemoveStatus(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}
	w := ref("widget", cid(1))

	if err := favErr(f.add(ctx, actor, ref("widget", cid(1)))); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !favorited(t, rt, "u1", w) {
		t.Fatal("status after add: want favorited")
	}
	if c := countsOf(t, rt, w); c.Favorites != 1 {
		t.Fatalf("favorites count after add = %d, want 1", c.Favorites)
	}
	// re-add is idempotent: no error, still a single row.
	if err := favErr(f.add(ctx, actor, ref("widget", cid(1)))); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if c := countsOf(t, rt, w); c.Favorites != 1 {
		t.Fatalf("favorites count after re-add = %d, want 1 (idempotent)", c.Favorites)
	}
	if err := favErr(f.remove(ctx, actor, ref("widget", cid(1)))); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if favorited(t, rt, "u1", w) {
		t.Fatal("status after remove: want not favorited")
	}
	if c := countsOf(t, rt, w); c.Favorites != 0 {
		t.Fatalf("favorites count after remove = %d, want 0", c.Favorites)
	}
	// remove again is idempotent (no row) -> no error.
	if err := favErr(f.remove(ctx, actor, ref("widget", cid(1)))); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}

func TestFavorites_TransactionErrorRollsBack(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, pool := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE `+rt.store.t.counts); err != nil {
		t.Fatalf("drop counts table: %v", err)
	}
	if err := favErr(rt.favorites.add(ctx, access.Actor{ID: "u1", Kind: "user"}, ref("widget", cid(1)))); err == nil {
		t.Fatal("favorite error = nil, want transaction failure")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.favorites).Scan(&n); err != nil || n != 0 {
		t.Fatalf("favorite rows after rollback = %d err=%v, want 0", n, err)
	}
}

// A visible but premium-locked target can be favorited, unlike a reaction
// which requires accessibility.
func TestFavorites_WishlistVisibleNotAccessible(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(904), true, false)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	if err := favErr(f.add(ctx, actor, ref("widget", cid(904)))); err != nil {
		t.Fatalf("favorite premium-locked: want success, got %v", err)
	}
	if !favorited(t, rt, "u1", ref("widget", cid(904))) {
		t.Fatal("status after favoriting premium-locked: want favorited")
	}
	if err := reactErr(rt.reactions.react(ctx, actor, rt.Ref("widget", cid(904)), 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react on premium-locked: want ErrForbidden, got %v", err)
	}
}

func TestFavorites_GatingHiddenMissing(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(902), false, false)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	if err := favErr(f.add(ctx, actor, ref("widget", cid(902)))); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("favorite hidden: want ErrNotVisible, got %v", err)
	}
	if err := favErr(f.add(ctx, actor, ref("widget", cid(903)))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("favorite missing: want ErrNotFound, got %v", err)
	}
	if err := favErr(f.add(ctx, actor, ref("unregistered", cid(1)))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("favorite unregistered kind: want ErrNotFound, got %v", err)
	}
}

func TestFavorites_AnonymousRejected(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	mux := http.NewServeMux()
	httpapi.Mount(mux, rt.guard.Guard, newFavorites(rt), favoriteRoutes())

	anon := access.Actor{Anonymous: true, IP: "10.0.0.9"}
	for _, c := range []struct{ method, path string }{
		{"POST", "/contents/" + cid(1) + "/favorite"}, {"DELETE", "/contents/" + cid(1) + "/favorite"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req = testauth.As(req, anon)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status %d, want 401 (body %s)", c.method, c.path, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest("POST", "/contents/"+cid(1)+"/favorite", nil)
	req = testauth.As(req, access.Actor{ID: "u1", Kind: "user"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated favorite: status %d, body %s", w.Code, w.Body.String())
	}
}

func TestFavorites_ListAndCounts(t *testing.T) {
	res := &fakeResolver{}
	for _, id := range []string{cid(1), cid(2), cid(3)} {
		res.set("widget", id, true, true)
	}
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	f := newFavorites(rt)
	ctx := context.Background()
	u1, u2 := access.Actor{ID: "u1", Kind: "user"}, access.Actor{ID: "u2", Kind: "user"}

	for _, id := range []string{cid(1), cid(2), cid(3)} {
		if err := favErr(f.add(ctx, u1, ref("widget", id))); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	if got, want := favoriteIDs(t, rt, "u1"), []string{cid(3), cid(2), cid(1)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("favorites %v, want %v (newest first)", got, want)
	}
	if err := favErr(f.add(ctx, u2, ref("widget", cid(1)))); err != nil {
		t.Fatalf("u2 add: %v", err)
	}
	counts, err := rt.Counts(ctx, []contentref.ContentRef{ref("widget", cid(1)), ref("widget", cid(2)), ref("widget", cid(3)), ref("widget", cid(4))})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts[ref("widget", cid(1)).Key()].Favorites != 2 || counts[ref("widget", cid(2)).Key()].Favorites != 1 || counts[ref("widget", cid(3)).Key()].Favorites != 1 {
		t.Fatalf("Counts favorites = %v, want 1:2 2:1 3:1", counts)
	}
	if _, ok := counts[ref("widget", cid(4)).Key()]; ok {
		t.Fatal("widget/4 has no engagement; it must be absent")
	}
}

// Interactions are gated by Visible and Accessible: an item the actor can
// only see takes a favorite, not a reaction.
func TestInteractionsGateOnAccessible(t *testing.T) {
	res := &fakeResolver{entries: map[string]resolve.Resolution{
		"widget:" + cid(1): {Visible: true},
		"widget:" + cid(2): {Visible: true, Accessible: true},
	}}
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}

	if err := favErr(newFavorites(rt).add(ctx, actor, ref("widget", cid(1)))); err != nil {
		t.Fatalf("favorite a visible item: %v", err)
	}
	if _, err := rt.reactions.react(ctx, actor, rt.Ref("widget", cid(1)), 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react without access: %v, want ErrForbidden", err)
	}
	if _, err := rt.reactions.react(ctx, actor, rt.Ref("widget", cid(2)), 1); err != nil {
		t.Fatalf("react with access: %v", err)
	}
}

// Every favorite answer carries the target's count, read under the canonical
// reference, where the caller sees it; signed out, a visible target only.
func TestFavoriteStateCarriesTheCount(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", cid(1), true, true)
	res.set("widget", cid(2), false, false)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("widget")})
	h := rt.Handler()
	state := func(actor access.Actor, method, path string, want FavoriteState) {
		t.Helper()
		rec := doJSON(t, h, actor, method, path, nil)
		var got FavoriteState
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %s as %q: %d %s, want %+v", method, path, actor.ID, rec.Code, rec.Body, want)
		}
	}
	u1, u2, anon := access.Actor{ID: "u1", Kind: "user"}, access.Actor{ID: "u2", Kind: "user"}, access.Actor{Anonymous: true, IP: "10.0.0.9"}
	item := "/contents/" + cid(1) + "/favorite"
	state(anon, "GET", item, FavoriteState{Count: ptr(0)})
	state(u1, "POST", item, FavoriteState{Favorited: true, Count: ptr(1)})
	state(u1, "POST", item, FavoriteState{Favorited: true, Count: ptr(1)})
	state(u2, "POST", item, FavoriteState{Favorited: true, Count: ptr(2)})
	state(anon, "GET", item, FavoriteState{Count: ptr(2)})
	state(u1, "GET", item, FavoriteState{Favorited: true, Count: ptr(2)})
	state(u1, "DELETE", item, FavoriteState{Count: ptr(1)})
	state(u2, "GET", item, FavoriteState{Favorited: true, Count: ptr(1)})
	if rec := doJSON(t, h, anon, "GET", "/contents/"+cid(2)+"/favorite", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("signed out, a hidden target: %d %s", rec.Code, rec.Body)
	}
	state(u1, "GET", "/contents/"+cid(2)+"/favorite", FavoriteState{})
}
