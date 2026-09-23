package content

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

func TestCounts_RollupAggregates(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "g1", true, true)
	res.set("gallery", "g2", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u1"}, "gallery", "g1", 1)))
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u2"}, "gallery", "g1", -1)))
	must(rt.favorites.add(ctx, Actor{ID: "u1"}, "gallery", "g1"))
	if _, err := rt.comments.create(ctx, Actor{ID: "u1"}, "gallery", "g1", createInput{Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u3"}, "gallery", "g2", 1)))

	if c := countsOf(t, rt, ref("gallery", "g1")); c.Likes != 1 || c.Dislikes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("g1 counts = %+v, want 1/1/1/1", c)
	}
	m, err := rt.Counts(ctx, []contentref.ContentRef{ref("gallery", "g1"), ref("gallery", "g2"), ref("gallery", "g3")})
	if err != nil {
		t.Fatal(err)
	}
	if m[ref("gallery", "g1").Key()].Likes != 1 || m[ref("gallery", "g2").Key()].Likes != 1 {
		t.Fatalf("batch counts = %+v", m)
	}
	if _, ok := m[ref("gallery", "g3").Key()]; ok {
		t.Fatal("g3 has no engagement; it should be absent from the batch map")
	}
	must(rt.favorites.remove(ctx, Actor{ID: "u1"}, "gallery", "g1"))
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u2"}, "gallery", "g1", 1)))
	if c := countsOf(t, rt, ref("gallery", "g1")); c.Favorites != 0 || c.Likes != 2 || c.Dislikes != 0 {
		t.Fatalf("after unfavorite + switch: %+v, want favorites=0 likes=2 dislikes=0", c)
	}
}

func TestMyReactions_BatchAndByActor(t *testing.T) {
	res := &fakeResolver{}
	for _, id := range []string{"t1", "t2", "t3"} {
		res.set("tag", id, true, true)
	}
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"tag"}})
	ctx := context.Background()
	u := Actor{ID: "u1"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(reactErr(rt.reactions.react(ctx, u, "tag", "t1", 1)))
	must(reactErr(rt.reactions.react(ctx, u, "tag", "t2", -1)))
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u2"}, "tag", "t3", 1)))

	t1, t2, t3 := ref("tag", "t1"), ref("tag", "t2"), ref("tag", "t3")
	m, err := rt.MyReactions(ctx, u, []contentref.ContentRef{t1, t2, t3})
	if err != nil {
		t.Fatal(err)
	}
	if m[t1.Key()] != 1 || m[t2.Key()] != -1 {
		t.Fatalf("MyReactions = %+v, want t1=1 t2=-1", m)
	}
	if _, ok := m[t3.Key()]; ok {
		t.Fatal("t3 belongs to another actor; it must be absent")
	}
	must(reactErr(rt.reactions.react(ctx, u, "tag", "t2", 0)))
	list, err := rt.ReactionsByActor(ctx, u, "tag", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].ContentRef.Equal(t1) || list[0].Value != 1 {
		t.Fatalf("ReactionsByActor = %+v, want only t1=1", list)
	}
	anon := Actor{Anonymous: true, IP: "10.0.0.9"}
	must(reactErr(rt.reactions.react(ctx, anon, "tag", "t3", 1)))
	am, err := rt.MyReactions(ctx, anon, []contentref.ContentRef{t3})
	if err != nil || am[t3.Key()] != 1 {
		t.Fatalf("anon MyReactions = %+v err=%v, want t3=1", am, err)
	}
}

func TestCounts_CommentCountLifecycle(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	a := Actor{ID: "u1"}
	g := ref("gallery", "1")

	top := mustComment(t, rt, a, "gallery", "1", createInput{Body: "top"})
	mustComment(t, rt, a, "gallery", "1", createInput{Body: "reply", ReplyToID: top.ID}) // reply: no rollup bump
	if c := countsOf(t, rt, g); c.CommentCount != 1 {
		t.Fatalf("comment_count = %d, want 1 (reply excluded)", c.CommentCount)
	}
	top2 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "top2"})
	if c := countsOf(t, rt, g); c.CommentCount != 2 {
		t.Fatalf("comment_count = %d, want 2", c.CommentCount)
	}
	if err := rt.comments.softDelete(ctx, a, top2.ID); err != nil {
		t.Fatal(err)
	}
	if c := countsOf(t, rt, g); c.CommentCount != 1 {
		t.Fatalf("comment_count after delete = %d, want 1", c.CommentCount)
	}
}

// Wilson "best": 9/1 outranks 1/0, and both outrank a no-vote comment.
func TestComments_SortByBest(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	a := Actor{ID: "author"}
	small := mustComment(t, rt, a, "gallery", "1", createInput{Body: "1/0"})
	big := mustComment(t, rt, a, "gallery", "1", createInput{Body: "9/1"})
	none := mustComment(t, rt, a, "gallery", "1", createInput{Body: "0/0"})

	if _, err := rt.comments.reactTx(ctx, Actor{ID: "v0"}, small.ID, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		if _, err := rt.comments.reactTx(ctx, Actor{ID: "u" + string(rune('a'+i))}, big.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.comments.reactTx(ctx, Actor{ID: "hater"}, big.ID, -1); err != nil {
		t.Fatal(err)
	}
	top, err := rt.comments.list(ctx, a, "gallery", "1", "best", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top[0].ID != big.ID || top[1].ID != small.ID || top[2].ID != none.ID {
		t.Fatalf("sort=best order = %v, want [9/1, 1/0, 0/0]", commentIDs(top))
	}
}

func TestComments_SortByLikes(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	a := Actor{ID: "author"}
	c1 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "c1"})
	c2 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "c2"})
	c3 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "c3"})
	for _, actor := range []Actor{{ID: "x1"}, {ID: "x2"}} {
		if _, err := rt.comments.reactTx(ctx, actor, c2.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.comments.reactTx(ctx, Actor{ID: "x1"}, c1.ID, 1); err != nil {
		t.Fatal(err)
	}
	top, err := rt.comments.list(ctx, a, "gallery", "1", "likes", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top[0].ID != c2.ID || top[1].ID != c1.ID || top[2].ID != c3.ID {
		t.Fatalf("sort=likes order = %v, want [c2 c1 c3]", commentIDs(top))
	}
}
