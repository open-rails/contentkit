package content

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/testauth"
)

// Two tenants share one host schema: every row, read, count and route of one
// is invisible to the other, and foreign references are refused, never remapped.
func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Resolver: res, Kinds: testKinds("gallery")})
	b, err := New(ctx, Options{Pool: pool, Schema: a.schema, Tenant: "site_b", Auth: testauth.Auth{Authz: allowAll{}}, Resolver: res, Kinds: testKinds("gallery")})
	if err != nil {
		t.Fatal(err)
	}
	u := access.Actor{ID: "shared-account", Kind: "user"}
	ga, gb := a.Ref("gallery", cid(1)), b.Ref("gallery", cid(1))

	if _, err := a.reactions.react(ctx, u, a.Ref("gallery", cid(1)), 1); err != nil {
		t.Fatal(err)
	}
	if err := favErr(a.favorites.add(ctx, u, a.Ref("gallery", cid(1)))); err != nil {
		t.Fatal(err)
	}
	cm := mustComment(t, a, u, "gallery", cid(1), CommentInput{Body: "on a"})
	poll, err := a.polls.create(ctx, a.as(u), PollInput{Question: "a?", Options: []PollOptionInput{{Label: "x"}, {Label: "y"}}})
	if err != nil {
		t.Fatal(err)
	}

	if c := countsOf(t, a, ga); c.Likes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("tenant a counts = %+v", c)
	}
	if m, err := b.Counts(ctx, []contentref.ContentRef{gb}); err != nil || len(m) != 0 {
		t.Fatalf("tenant b sees a's counts: %v err=%v", m, err)
	}
	if reactionOf(t, b, u, gb) != 0 || favorited(t, b, u.ID, gb) {
		t.Fatal("tenant b sees a's reaction or favorite")
	}
	if list, _ := b.comments.list(ctx, u, b.Ref("gallery", cid(1)), "", "", 10, 0); len(list) != 0 {
		t.Fatalf("tenant b sees a's comments: %v", list)
	}
	if feed, _ := b.LatestComments(ctx, u, "", 10, 0); len(feed) != 0 {
		t.Fatalf("tenant b's feed shows a's comments: %v", feed)
	}
	if n, _ := b.LatestCommentsTotal(ctx); n != 0 {
		t.Fatalf("tenant b's feed total counts a's comments: %d", n)
	}
	if m, _ := a.CommentReactionsByAuthor(ctx, []string{u.ID}); len(m) != 1 {
		t.Fatalf("tenant a author totals = %v, want the shared account's own row", m)
	}
	if m, _ := b.CommentReactionsByAuthor(ctx, []string{u.ID}); len(m) != 0 {
		t.Fatalf("tenant b sees a's author totals: %v", m)
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
	req = testauth.As(req, u)
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("tenant b edited a's poll option")
	}
	// Cross-tenant references are refused on every read API.
	for name, err := range map[string]error{
		"Counts": errOf(b.Counts(ctx, []contentref.ContentRef{ga})),
	} {
		if !errors.Is(err, ErrTenant) {
			t.Fatalf("%s with a foreign reference: want ErrTenant, got %v", name, err)
		}
	}
	// The shared account's tenant-b wishlist is its own.
	if err := favErr(b.favorites.add(ctx, u, b.Ref("gallery", cid(1)))); err != nil {
		t.Fatal(err)
	}
	if !favorited(t, a, u.ID, ga) || len(favoriteIDs(t, a, u.ID)) != 1 {
		t.Fatal("tenant a's favorite is not its own")
	}
	if c := countsOf(t, b, gb); c.Favorites != 1 || c.Likes != 0 {
		t.Fatalf("tenant b counts = %+v, want its own favorite only", c)
	}
}

func errOf[T any](_ T, err error) error { return err }
