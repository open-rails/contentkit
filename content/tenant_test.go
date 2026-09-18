package content

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

// Two tenants share one host schema: every row, read, count and route of one
// is invisible to the other, and foreign references are refused, never remapped.
func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	res := &fakeResolver{}
	res.set("gallery", "g1", true, true)
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Resolver: res, ContentKinds: []string{"gallery"}, Perms: Perms{PollWrite: "poll", PostWrite: "post"}})
	b, err := New(ctx, Options{Pool: pool, Schema: a.schema, Tenant: "site_b", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: res, ContentKinds: []string{"gallery"}, Perms: Perms{PollWrite: "poll", PostWrite: "post"}})
	if err != nil {
		t.Fatal(err)
	}
	u := Actor{ID: "shared-account", Kind: "user"}
	ga, gb := a.Ref("gallery", "g1"), b.Ref("gallery", "g1")

	if _, _, err := a.reactions.react(ctx, u, "gallery", "g1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.favorites.add(ctx, u, "gallery", "g1"); err != nil {
		t.Fatal(err)
	}
	cm := mustComment(t, a, u, "gallery", "g1", createInput{Body: "on a"})
	poll, err := a.polls.create(ctx, u, createPollInput{Question: "a?", Options: []createOptionInput{{Label: "x"}, {Label: "y"}}})
	if err != nil {
		t.Fatal(err)
	}

	if c := countsOf(t, a, ga); c.Likes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("tenant a counts = %+v", c)
	}
	if m, err := b.Counts(ctx, []contentref.ContentRef{gb}); err != nil || len(m) != 0 {
		t.Fatalf("tenant b sees a's counts: %v err=%v", m, err)
	}
	if m, _ := b.MyReactions(ctx, u, []contentref.ContentRef{gb}); len(m) != 0 {
		t.Fatalf("tenant b sees a's reaction: %v", m)
	}
	if m, _ := b.IsFavorited(ctx, u.ID, []contentref.ContentRef{gb}); m[gb.Key()] {
		t.Fatal("tenant b sees a's favorite")
	}
	if list, _ := b.comments.list(ctx, u, "gallery", "g1", "", 10, 0); len(list) != 0 {
		t.Fatalf("tenant b sees a's comments: %v", list)
	}
	if feed, _ := b.LatestComments(ctx, u, 10, 0); len(feed) != 0 {
		t.Fatalf("tenant b's feed shows a's comments: %v", feed)
	}
	if n, _ := b.LatestCommentsTotal(ctx); n != 0 {
		t.Fatalf("tenant b's feed total counts a's comments: %d", n)
	}
	if _, err := b.comments.edit(ctx, u, cm.ID, "hijack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b edits a's comment: %v", err)
	}
	if _, err := b.comments.reactTx(ctx, u, cm.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b reacts to a's comment: %v", err)
	}
	if _, err := b.polls.get(ctx, u, poll.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b reads a's poll: %v", err)
	}
	if _, err := b.polls.vote(ctx, u, poll.ID, poll.Options[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b votes on a's poll: %v", err)
	}
	if lst, _ := b.polls.list(ctx, u, listFilter{limit: 10}); len(lst) != 0 {
		t.Fatalf("tenant b lists a's polls: %v", lst)
	}
	req := httptest.NewRequest("PATCH", "/polls/"+poll.ID+"/options/"+poll.Options[0].ID, nil)
	req = req.WithContext(withActor(req.Context(), u))
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("tenant b edited a's poll option")
	}
	// Cross-tenant references are refused on every read API.
	for name, err := range map[string]error{
		"Counts":      errOf(b.Counts(ctx, []contentref.ContentRef{ga})),
		"MyReactions": errOf(b.MyReactions(ctx, u, []contentref.ContentRef{ga})),
		"IsFavorited": errOf(b.IsFavorited(ctx, u.ID, []contentref.ContentRef{ga})),
	} {
		if !errors.Is(err, ErrTenant) {
			t.Fatalf("%s with a foreign reference: want ErrTenant, got %v", name, err)
		}
	}
	// The shared account's tenant-b wishlist is its own.
	if _, err := b.favorites.add(ctx, u, "gallery", "g1"); err != nil {
		t.Fatal(err)
	}
	if items, _ := a.ListFavorites(ctx, u.ID, 0, 0); len(items) != 1 || items[0].TenantID != "site_a" {
		t.Fatalf("tenant a favorites = %v", items)
	}
	if c := countsOf(t, b, gb); c.Favorites != 1 || c.Likes != 0 {
		t.Fatalf("tenant b counts = %+v, want its own favorite only", c)
	}
}

func errOf[T any](_ T, err error) error { return err }
