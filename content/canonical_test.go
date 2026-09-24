package content

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// aliasResolver canonicalizes any alias of gallery 123 ("slug-123", "123") to
// the composite id "123:en"; the tenant is left for the runtime to pin.
type aliasResolver struct{}

func (aliasResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		switch r.ContentID {
		case "slug-123", "123", "123:en":
			out[r.Key()] = access.Resolution{Ref: contentref.New("", r.ContentKind, "123:en"), Visible: true, Accessible: true}
		}
	}
	return out, nil
}

// Writes through any alias land on one canonical row; reads through any alias
// see it.
func TestCanonicalRef_UnifiesAliases(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{Resolver: aliasResolver{}, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	u := access.Actor{ID: "u1"}
	canonical := ref("gallery", "123:en")

	if err := rt.favorites.add(ctx, u, "gallery", "slug-123"); err != nil {
		t.Fatalf("add via alias: %v", err)
	}
	fav, err := rt.IsFavorited(ctx, "u1", []contentref.ContentRef{canonical})
	if err != nil || !fav[canonical.Key()] {
		t.Fatalf("favorite via alias not stored under the canonical reference: %v %v", fav, err)
	}
	if _, err := rt.reactions.react(ctx, u, "gallery", "123", 1); err != nil {
		t.Fatalf("react via alias: %v", err)
	}
	if _, err := rt.comments.create(ctx, u, "gallery", "123:en", createInput{Body: "hi"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	list, err := rt.comments.list(ctx, u, "gallery", "slug-123", "", 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("comment list via alias = %d rows err=%v, want 1", len(list), err)
	}
	if c := countsOf(t, rt, canonical); c.Likes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("canonical rollup = %+v, want 1/1/1", c)
	}
	if alias := countsOf(t, rt, ref("gallery", "slug-123")); alias != (Counts{}) {
		t.Fatalf("alias key leaked into the rollup: %+v", alias)
	}
	if err := rt.favorites.remove(ctx, u, "gallery", "123"); err != nil {
		t.Fatal(err)
	}
	if c := countsOf(t, rt, canonical); c.Favorites != 0 {
		t.Fatalf("favorites after aliased remove = %d, want 0", c.Favorites)
	}
}

// versionResolver maps "g1@v2" to version v2 of g1: explicit version feedback
// is a distinct key from the work's.
type versionResolver struct{}

func (versionResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		switch r.ContentID {
		case "g1":
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true}
		case "g1@v2":
			out[r.Key()] = access.Resolution{Ref: contentref.NewVersion(r.TenantID, r.ContentKind, "g1", "v2"), Visible: true, Accessible: true}
		}
	}
	return out, nil
}

func TestCanonicalRef_VersionIsADistinctKey(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{Resolver: versionResolver{}, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	u := access.Actor{ID: "u1"}
	work, version := ref("gallery", "g1"), contentref.NewVersion(testTenant, "gallery", "g1", "v2")

	if _, err := rt.reactions.react(ctx, u, "gallery", "g1", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := rt.reactions.react(ctx, u, "gallery", "g1@v2", -1); err != nil || !got.Equal(version) {
		t.Fatalf("version react = %s err=%v", got, err)
	}
	mine, err := rt.MyReactions(ctx, u, []contentref.ContentRef{work, version})
	if err != nil || mine[work.Key()] != 1 || mine[version.Key()] != -1 {
		t.Fatalf("MyReactions = %v err=%v, want work=1 version=-1", mine, err)
	}
	if c := countsOf(t, rt, work); c.Likes != 1 || c.Dislikes != 0 {
		t.Fatalf("work counts = %+v", c)
	}
	if c := countsOf(t, rt, version); c.Likes != 0 || c.Dislikes != 1 {
		t.Fatalf("version counts = %+v", c)
	}
}

// foreignResolver answers with another tenant's reference.
type foreignResolver struct{}

func (foreignResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		out[r.Key()] = access.Resolution{Ref: contentref.New("other", r.ContentKind, r.ContentID), Visible: true, Accessible: true}
	}
	return out, nil
}

func TestCanonicalRef_ForeignTenantIsRefused(t *testing.T) {
	rt, pool := newTestRuntime(t, Options{Resolver: foreignResolver{}, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	if err := reactErr(rt.reactions.react(ctx, access.Actor{ID: "u1"}, "gallery", "g1", 1)); !errors.Is(err, ErrTenant) {
		t.Fatalf("react: want ErrTenant, got %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.reactions).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows written for a foreign tenant = %d err=%v", n, err)
	}
}

func TestRuntime_ListFavoritesExported(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "a", true, true)
	res.set("gallery", "b", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	u := access.Actor{ID: "u1"}
	for _, id := range []string{"a", "b"} {
		if err := rt.favorites.add(ctx, u, "gallery", id); err != nil {
			t.Fatal(err)
		}
	}
	items, err := rt.ListFavorites(ctx, "u1", 0, 0) // limit <= 0 = all
	if err != nil || len(items) != 2 || items[0].ContentID != "b" {
		t.Fatalf("ListFavorites = %+v err=%v, want [b, a]", items, err)
	}
	if one, err := rt.ListFavorites(ctx, "u1", 1, 0); err != nil || len(one) != 1 {
		t.Fatalf("paged ListFavorites = %+v (%v), want 1 row", one, err)
	}
}
