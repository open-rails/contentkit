package content

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

// favIsFavorited is the batch check narrowed to one target for terse assertions.
func favIsFavorited(t *testing.T, f *favorites, userID string, r contentref.ContentRef) bool {
	t.Helper()
	m, err := f.IsFavorited(context.Background(), userID, []contentref.ContentRef{r})
	if err != nil {
		t.Fatalf("IsFavorited: %v", err)
	}
	return m[r.Key()]
}

func TestFavorites_AddRemoveStatus(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	w := ref("widget", "1")

	if _, err := f.add(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !favIsFavorited(t, f, "u1", w) {
		t.Fatal("status after add: want favorited")
	}
	if c := countsOf(t, rt, w); c.Favorites != 1 {
		t.Fatalf("favorites count after add = %d, want 1", c.Favorites)
	}
	// re-add is idempotent: no error, still a single row.
	if _, err := f.add(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if c := countsOf(t, rt, w); c.Favorites != 1 {
		t.Fatalf("favorites count after re-add = %d, want 1 (idempotent)", c.Favorites)
	}
	if _, err := f.remove(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if favIsFavorited(t, f, "u1", w) {
		t.Fatal("status after remove: want not favorited")
	}
	if c := countsOf(t, rt, w); c.Favorites != 0 {
		t.Fatalf("favorites count after remove = %d, want 0", c.Favorites)
	}
	// remove again is idempotent (no row) -> no error.
	if _, err := f.remove(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}

func TestFavorites_TransactionErrorRollsBack(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, pool := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE `+rt.store.t.counts); err != nil {
		t.Fatalf("drop counts table: %v", err)
	}
	if _, err := rt.favorites.add(ctx, Actor{ID: "u1", Kind: "user"}, "widget", "1"); err == nil {
		t.Fatal("favorite error = nil, want transaction failure")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.favorites).Scan(&n); err != nil || n != 0 {
		t.Fatalf("favorite rows after rollback = %d err=%v, want 0", n, err)
	}
}

func TestFavorites_BatchIsFavorited(t *testing.T) {
	res := &fakeResolver{}
	for _, id := range []string{"1", "2", "3"} {
		res.set("widget", id, true, true)
	}
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	if _, err := f.add(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("add 1: %v", err)
	}
	if _, err := f.add(ctx, actor, "widget", "3"); err != nil {
		t.Fatalf("add 3: %v", err)
	}
	targets := []contentref.ContentRef{ref("widget", "1"), ref("widget", "2"), ref("widget", "3"), ref("widget", "4")}
	got, err := f.IsFavorited(ctx, "u1", targets)
	if err != nil {
		t.Fatalf("IsFavorited: %v", err)
	}
	want := map[contentref.ContentKey]bool{
		ref("widget", "1").Key(): true, ref("widget", "2").Key(): false,
		ref("widget", "3").Key(): true, ref("widget", "4").Key(): false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IsFavorited = %v, want %v", got, want)
	}
	// A reference of another tenant is refused, never remapped.
	if _, err := f.IsFavorited(ctx, "u1", []contentref.ContentRef{contentref.New("other", "widget", "1")}); !errors.Is(err, ErrTenant) {
		t.Fatalf("foreign tenant: want ErrTenant, got %v", err)
	}
}

// A visible but premium-locked target can be favorited, unlike a reaction
// which requires accessibility.
func TestFavorites_WishlistVisibleNotAccessible(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "premium", true, false)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	if _, err := f.add(ctx, actor, "widget", "premium"); err != nil {
		t.Fatalf("favorite premium-locked: want success, got %v", err)
	}
	if !favIsFavorited(t, f, "u1", ref("widget", "premium")) {
		t.Fatal("status after favoriting premium-locked: want favorited")
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "widget", "premium", 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react on premium-locked: want ErrForbidden, got %v", err)
	}
}

func TestFavorites_GatingHiddenMissing(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "hidden", false, false)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	if _, err := f.add(ctx, actor, "widget", "hidden"); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("favorite hidden: want ErrNotVisible, got %v", err)
	}
	if _, err := f.add(ctx, actor, "widget", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("favorite missing: want ErrNotFound, got %v", err)
	}
	if _, err := f.add(ctx, actor, "unregistered", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("favorite unregistered kind: want ErrNotFound, got %v", err)
	}
}

func TestFavorites_AnonymousRejected(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	mux := http.NewServeMux()
	newFavorites(rt).mount(mux)

	anon := Actor{Anonymous: true, IP: "10.0.0.9"}
	for _, c := range []struct{ method, path string }{
		{"POST", "/widget/1/favorite"}, {"DELETE", "/widget/1/favorite"}, {"GET", "/widget/1/favorite"}, {"GET", "/favorites"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req = req.WithContext(withActor(req.Context(), anon))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status %d, want 401 (body %s)", c.method, c.path, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest("POST", "/widget/1/favorite", nil)
	req = req.WithContext(withActor(req.Context(), Actor{ID: "u1", Kind: "user"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated favorite: status %d, body %s", w.Code, w.Body.String())
	}
}

func TestFavorites_ListAndCounts(t *testing.T) {
	res := &fakeResolver{}
	for _, id := range []string{"1", "2", "3"} {
		res.set("widget", id, true, true)
	}
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	u1, u2 := Actor{ID: "u1", Kind: "user"}, Actor{ID: "u2", Kind: "user"}

	for _, id := range []string{"1", "2", "3"} {
		if _, err := f.add(ctx, u1, "widget", id); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	items, err := f.list(ctx, "u1", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var order []string
	for _, it := range items {
		if it.TenantID != testTenant || it.ContentKind != "widget" {
			t.Fatalf("favorite item reference = %s", it.ContentRef)
		}
		order = append(order, it.ContentID)
	}
	if want := []string{"3", "2", "1"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("list order = %v, want %v (newest-first)", order, want)
	}
	if _, err := f.add(ctx, u2, "widget", "1"); err != nil {
		t.Fatalf("u2 add: %v", err)
	}
	counts, err := rt.Counts(ctx, []contentref.ContentRef{ref("widget", "1"), ref("widget", "2"), ref("widget", "3"), ref("widget", "4")})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts[ref("widget", "1").Key()].Favorites != 2 || counts[ref("widget", "2").Key()].Favorites != 1 || counts[ref("widget", "3").Key()].Favorites != 1 {
		t.Fatalf("Counts favorites = %v, want 1:2 2:1 3:1", counts)
	}
	if _, ok := counts[ref("widget", "4").Key()]; ok {
		t.Fatal("widget/4 has no engagement; it must be absent")
	}
	page, err := f.list(ctx, "u1", 2, 0)
	if err != nil || len(page) != 2 || page[0].ContentID != "3" || page[1].ContentID != "2" {
		t.Fatalf("paged list = %v err=%v, want [3 2]", page, err)
	}
}
