package content

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/access"
)

// commentsEnricher is a fake UserEnricher for the enrichment assertion.
type commentsEnricher struct{}

func (commentsEnricher) UsersByIDs(_ context.Context, ids []string) (map[string]PublicUser, error) {
	out := make(map[string]PublicUser, len(ids))
	for _, id := range ids {
		out[id] = PublicUser{ID: id, Username: "name-" + id}
	}
	return out, nil
}

func commentsRuntime(t *testing.T, opts Options) *Runtime {
	res := &fakeResolver{}
	res.set("gallery", "1", true, true)
	if opts.Resolver == nil {
		opts.Resolver = res
	}
	if opts.ContentKinds == nil {
		opts.ContentKinds = []string{"gallery"}
	}
	rt, _ := newTestRuntime(t, opts)
	return rt
}

func mustComment(t *testing.T, rt *Runtime, actor access.Actor, kind, id string, in createInput) Comment {
	t.Helper()
	cm, err := rt.comments.create(context.Background(), actor, kind, id, in)
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	return cm
}

func TestComments_TopLevelRepliesAndReplyCount(t *testing.T) {
	rt := commentsRuntime(t, Options{Users: commentsEnricher{}})
	ctx := context.Background()
	author := access.Actor{ID: "author"}

	a := mustComment(t, rt, author, "gallery", "1", createInput{Body: "root A"})
	r := mustComment(t, rt, author, "gallery", "1", createInput{Body: "reply to A", ReplyToID: a.ID})
	_ = mustComment(t, rt, author, "gallery", "1", createInput{Body: "root B"})
	if r.ReplyToID != a.ID {
		t.Fatalf("reply_to_id = %q, want %q", r.ReplyToID, a.ID)
	}
	top, err := rt.comments.list(ctx, author, "gallery", "1", "", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("top-level count = %d, want 2 (reply excluded)", len(top))
	}
	if indexOfComment(top, r.ID) >= 0 {
		t.Fatal("a reply leaked into the top-level list")
	}
	ai := indexOfComment(top, a.ID)
	if ai < 0 || top[ai].ReplyCount != 1 {
		t.Fatalf("root A = %+v, want reply_count 1", top)
	}
	if top[ai].Author == nil || top[ai].Author.Username != "name-author" {
		t.Fatalf("author not enriched: %+v", top[ai].Author)
	}
	reps, err := rt.comments.replies(ctx, author, a.ID, 10, 0)
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	if len(reps) != 1 || reps[0].ID != r.ID || reps[0].ReplyToID != a.ID {
		t.Fatalf("replies = %+v, want [reply %s]", commentIDs(reps), r.ID)
	}
}

func TestComments_ReplyConstraints(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "1", true, true)
	res.set("gallery", "2", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	author := access.Actor{ID: "author"}

	on1 := mustComment(t, rt, author, "gallery", "1", createInput{Body: "on content 1"})
	if _, err := rt.comments.create(ctx, author, "gallery", "2", createInput{Body: "cross", ReplyToID: on1.ID}); err == nil {
		t.Fatal("expected rejection: replied-to comment belongs to different content")
	}
	reply := mustComment(t, rt, author, "gallery", "1", createInput{Body: "reply", ReplyToID: on1.ID})
	if _, err := rt.comments.create(ctx, author, "gallery", "1", createInput{Body: "nested", ReplyToID: reply.ID}); err == nil {
		t.Fatal("expected rejection: cannot reply to a reply")
	}
}

func TestComments_AccessGating(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "locked", true, false)
	res.set("gallery", "hidden", false, false)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	author := access.Actor{ID: "author"}

	if _, err := rt.comments.create(ctx, author, "gallery", "locked", createInput{Body: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("premium-locked: want ErrForbidden, got %v", err)
	}
	if _, err := rt.comments.create(ctx, author, "gallery", "hidden", createInput{Body: "x"}); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("hidden: want ErrNotVisible, got %v", err)
	}
	if _, err := rt.comments.create(ctx, author, "gallery", "ghost", createInput{Body: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}
}

// Edits run the same sanitizer as create, and a policy rejection (the C4
// moderator seam) answers 422 with its reason.
func TestComments_EditSanitizesAndRejectionIs422(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	cm := mustComment(t, rt, author, "gallery", "1", createInput{Body: "<b>fine</b>"})
	if cm.Body != "fine" {
		t.Fatalf("create body = %q, want tags stripped", cm.Body)
	}
	edited, err := rt.comments.edit(ctx, author, cm.ID, "<i>still</i> fine")
	if err != nil || edited.Body != "still fine" {
		t.Fatalf("edit = %+v err=%v, want the sanitized body", edited, err)
	}
	if _, err := rt.comments.edit(ctx, author, cm.ID, "<script></script>"); err == nil {
		t.Fatal("an edit that sanitizes to nothing must be rejected")
	}
	rec := httptest.NewRecorder()
	writeErr(rec, RejectedError{Reason: "links are not allowed"})
	if rec.Code != http.StatusUnprocessableEntity || rec.Body.String() != "{\"error\":\"links are not allowed\",\"code\":\"moderation_rejected\"}\n" {
		t.Fatalf("rejection = %d %s, want 422 with the reason and the moderation code", rec.Code, rec.Body.String())
	}
}

func TestComments_AnonRequiresName(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	anon := access.Actor{IP: "1.2.3.4", Anonymous: true}
	if _, err := rt.comments.create(ctx, anon, "gallery", "1", createInput{Body: "hi"}); err == nil {
		t.Fatal("anon without a name should be rejected")
	}
	if _, err := rt.comments.create(ctx, anon, "gallery", "1", createInput{Body: "hi", AnonName: "Guest"}); err != nil {
		t.Fatalf("named anon should be allowed: %v", err)
	}
}

func TestComments_SoftDeleteKeepsThread(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	top := mustComment(t, rt, author, "gallery", "1", createInput{Body: "top"})
	reply := mustComment(t, rt, author, "gallery", "1", createInput{Body: "reply", ReplyToID: top.ID})

	if err := rt.comments.softDelete(ctx, author, top.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	list, err := rt.comments.list(ctx, author, "gallery", "1", "", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || !list[0].Deleted || list[0].Body != commentTombstone {
		t.Fatalf("tombstone = %+v", list)
	}
	reps, err := rt.comments.replies(ctx, author, top.ID, 10, 0)
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	if indexOfComment(reps, reply.ID) < 0 {
		t.Fatal("reply disappeared after soft-delete of the replied-to comment")
	}
}

func TestComments_DeleteOwnerAndModerator(t *testing.T) {
	denyRt, _ := newTestRuntime(t, Options{
		Resolver: resolverWith("gallery", "1"), ContentKinds: []string{"gallery"},
		Authz: denyAll{}, Perms: Perms{CommentModerate: "root:comment:moderate"},
	})
	ctx := context.Background()
	c1 := mustComment(t, denyRt, access.Actor{ID: "author"}, "gallery", "1", createInput{Body: "mine"})
	if err := denyRt.comments.softDelete(ctx, access.Actor{ID: "author"}, c1.ID); err != nil {
		t.Fatalf("owner delete should succeed: %v", err)
	}
	c2 := mustComment(t, denyRt, access.Actor{ID: "author"}, "gallery", "1", createInput{Body: "mine2"})
	if err := denyRt.comments.softDelete(ctx, access.Actor{ID: "intruder"}, c2.ID); !errors.Is(err, errForbidden) {
		t.Fatalf("non-owner without perm: want forbidden, got %v", err)
	}
	modRt, _ := newTestRuntime(t, Options{
		Resolver: resolverWith("gallery", "1"), ContentKinds: []string{"gallery"},
		Authz: allowAll{}, Perms: Perms{CommentModerate: "root:comment:moderate"},
	})
	c3 := mustComment(t, modRt, access.Actor{ID: "author"}, "gallery", "1", createInput{Body: "theirs"})
	if err := modRt.comments.softDelete(ctx, access.Actor{ID: "mod"}, c3.ID); err != nil {
		t.Fatalf("moderator delete should succeed: %v", err)
	}
}

func TestComments_ReactionCountersExact(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	cm := mustComment(t, rt, author, "gallery", "1", createInput{Body: "react to me"})
	reactor := access.Actor{ID: "reactor"}

	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.comments.reactTx(context.Background(), reactor, cm.ID, 1)
		}()
	}
	wg.Wait()
	top, err := rt.comments.list(ctx, reactor, "gallery", "1", "", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	i := indexOfComment(top, cm.ID)
	if top[i].Likes != 1 || top[i].Dislikes != 0 || top[i].Mine != 1 {
		t.Fatalf("comment = %+v, want likes 1 dislikes 0 mine 1", top[i])
	}
	if _, err := rt.comments.reactTx(ctx, reactor, cm.ID, -1); err != nil {
		t.Fatalf("switch reaction: %v", err)
	}
	top, _ = rt.comments.list(ctx, reactor, "gallery", "1", "", 10, 0)
	i = indexOfComment(top, cm.ID)
	if top[i].Likes != 0 || top[i].Dislikes != 1 {
		t.Fatalf("after switch: likes %d dislikes %d, want 0/1", top[i].Likes, top[i].Dislikes)
	}

	// Same counters, totalled per author: only live published comments
	// contribute and an anonymous comment has no author to credit.
	second := mustComment(t, rt, author, "gallery", "1", createInput{Body: "and me"})
	if _, err := rt.comments.reactTx(ctx, access.Actor{ID: "other"}, second.ID, 1); err != nil {
		t.Fatalf("react to second comment: %v", err)
	}
	anon := mustComment(t, rt, access.Actor{Anonymous: true, IP: "9.9.9.9"}, "gallery", "1", createInput{Body: "anon", AnonName: "guest"})
	if _, err := rt.comments.reactTx(ctx, reactor, anon.ID, 1); err != nil {
		t.Fatalf("react to anonymous comment: %v", err)
	}
	totals, err := rt.CommentReactionsByAuthor(ctx, []string{author.ID, author.ID, "nobody"})
	if err != nil {
		t.Fatalf("CommentReactionsByAuthor: %v", err)
	}
	if len(totals) != 1 || totals[author.ID] != (AuthorReactions{Likes: 1, Dislikes: 1}) {
		t.Fatalf("author totals = %+v, want only author with 1 like and 1 dislike", totals)
	}
	if err := rt.comments.softDelete(ctx, author, second.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if totals, _ = rt.CommentReactionsByAuthor(ctx, []string{author.ID}); totals[author.ID] != (AuthorReactions{Dislikes: 1}) {
		t.Fatalf("totals after tombstone = %+v, want the deleted comment's like gone", totals)
	}
}

func TestComments_ReplyCountDecrementsOnDelete(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	top := mustComment(t, rt, author, "gallery", "1", createInput{Body: "p"})
	r1 := mustComment(t, rt, author, "gallery", "1", createInput{Body: "r1", ReplyToID: top.ID})
	_ = mustComment(t, rt, author, "gallery", "1", createInput{Body: "r2", ReplyToID: top.ID})

	list, _ := rt.comments.list(ctx, author, "gallery", "1", "", 10, 0)
	if got := list[indexOfComment(list, top.ID)].ReplyCount; got != 2 {
		t.Fatalf("reply_count = %d, want 2", got)
	}
	if err := rt.comments.softDelete(ctx, author, r1.ID); err != nil {
		t.Fatalf("delete reply: %v", err)
	}
	list, _ = rt.comments.list(ctx, author, "gallery", "1", "", 10, 0)
	if got := list[indexOfComment(list, top.ID)].ReplyCount; got != 1 {
		t.Fatalf("reply_count after reply delete = %d, want 1", got)
	}
}

// --- helpers ---

func resolverWith(kind, id string) *fakeResolver {
	r := &fakeResolver{}
	r.set(kind, id, true, true)
	return r
}

func indexOfComment(list []Comment, id string) int {
	for i := range list {
		if list[i].ID == id {
			return i
		}
	}
	return -1
}

func commentIDs(list []Comment) []string {
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	return ids
}

func TestComments_LatestFeed(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "1", true, true)
	res.set("gallery", "2", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}, Users: commentsEnricher{}})
	ctx := context.Background()
	a := access.Actor{ID: "author"}

	c1 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "on g1"})
	c2 := mustComment(t, rt, a, "gallery", "2", createInput{Body: "on g2"})
	gone := mustComment(t, rt, a, "gallery", "1", createInput{Body: "deleted later"})
	if err := rt.comments.softDelete(ctx, a, gone.ID); err != nil {
		t.Fatal(err)
	}
	feed, err := rt.comments.latest(ctx, a, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || feed[0].ID != c2.ID || feed[1].ID != c1.ID {
		t.Fatalf("feed = %+v, want [c2, c1]", feed)
	}
	if !feed[0].ContentRef.Equal(ref("gallery", "2")) {
		t.Fatalf("feed[0] reference = %s, want %s", feed[0].ContentRef, ref("gallery", "2"))
	}
	if feed[0].Author == nil || feed[0].Author.Username != "name-author" {
		t.Fatalf("feed author not enriched: %+v", feed[0].Author)
	}
	res.set("gallery", "2", false, false)
	feed, err = rt.comments.latest(ctx, a, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 1 || feed[0].ID != c1.ID {
		t.Fatalf("feed after hide = %v, want [c1]", commentFeedIDs(feed))
	}
}

// A full /comments/latest page resolves all its distinct references in one
// resolver call and keeps only the visible ones.
func TestComments_LatestResolvesPageOnce(t *testing.T) {
	res := &fakeResolver{}
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	a := access.Actor{ID: "author"}
	for i := range 40 {
		id := strconv.Itoa(i)
		res.set("gallery", id, true, true)
		mustComment(t, rt, a, "gallery", id, createInput{Body: "one on " + id})
		mustComment(t, rt, a, "gallery", id, createInput{Body: "two on " + id})
		if i%4 == 0 {
			res.set("gallery", id, false, false)
		} else if i%4 == 1 {
			delete(res.entries, "gallery:"+id)
		}
	}
	res.calls = 0
	rec := doJSON(t, rt.Handler(), a, "GET", "/comments/latest?limit=100", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("latest: %d %s", rec.Code, rec.Body)
	}
	var feed []FeedItem
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	if res.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", res.calls)
	}
	if len(feed) != 40 {
		t.Fatalf("feed has %d items, want 40 (two on each of 20 visible galleries)", len(feed))
	}
	for _, it := range feed {
		if n, _ := strconv.Atoi(it.ContentID); n%4 < 2 {
			t.Fatalf("hidden gallery %s in feed", it.ContentID)
		}
	}
}

// The feed total counts exactly the rows the feed draws from: live and
// approved, across content, never a held, rejected or tombstoned one.
func TestComments_LatestFeedTotal(t *testing.T) {
	mod := &fakeModerator{}
	rt := moderatedRuntime(t, mod)
	ctx := context.Background()
	a := access.Actor{ID: "author"}

	if n, err := rt.LatestCommentsTotal(ctx); err != nil || n != 0 {
		t.Fatalf("empty total = %d, %v; want 0", n, err)
	}

	_ = mustComment(t, rt, a, "gallery", "1", createInput{Body: "approved one"})
	_ = mustComment(t, rt, a, "gallery", "1", createInput{Body: "approved two"})
	if held := mustComment(t, rt, a, "gallery", "1", createInput{Body: "iffy remark"}); held.Moderation != ModerationHeld {
		t.Fatalf("fixture not held: %+v", held)
	}
	rejected := mustComment(t, rt, a, "gallery", "1", createInput{Body: "iffy offer"})
	if err := rt.Resolve(ctx, KindComment, rejected.ID, ReviewDecision{Revision: 1, Decision: DecisionReject, Reviewer: "reviewer"}); err != nil {
		t.Fatalf("reject fixture: %v", err)
	}
	gone := mustComment(t, rt, a, "gallery", "1", createInput{Body: "deleted later"})
	if err := rt.comments.softDelete(ctx, a, gone.ID); err != nil {
		t.Fatal(err)
	}

	total, err := rt.LatestCommentsTotal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	feed, err := rt.LatestComments(ctx, a, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(feed) != 2 {
		t.Fatalf("total = %d, feed = %v; want 2 and two items", total, commentFeedIDs(feed))
	}
}

func commentFeedIDs(items []FeedItem) []string {
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	return ids
}

func TestComments_AdminListAndRestore(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{
		Resolver: resolverWith("gallery", "1"), ContentKinds: []string{"gallery"},
		Authz: pollAdminOnly{}, Perms: Perms{CommentModerate: "root:comments:delete"},
	})
	ctx := context.Background()
	admin, user := access.Actor{ID: "admin"}, access.Actor{ID: "user1"}

	top := mustComment(t, rt, user, "gallery", "1", createInput{Body: "visible"})
	hidden := mustComment(t, rt, user, "gallery", "1", createInput{Body: "hide me"})
	if err := rt.comments.softDelete(ctx, admin, hidden.ID); err != nil {
		t.Fatal(err)
	}
	items, err := rt.comments.adminList(ctx, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("admin list = %d rows, want 2", len(items))
	}
	var del *AdminComment
	for i := range items {
		if items[i].ID == hidden.ID {
			del = &items[i]
		}
	}
	if del == nil || !del.Deleted || del.Body != "hide me" || del.DeletedAt == nil || !del.ContentRef.Equal(ref("gallery", "1")) {
		t.Fatalf("deleted row wrong in admin view: %+v", del)
	}
	if only, _ := rt.comments.adminList(ctx, "video", 10, 0); len(only) != 0 {
		t.Fatalf("kind filter returned %d rows, want 0", len(only))
	}
	if err := rt.comments.restore(ctx, hidden.ID); err != nil {
		t.Fatal(err)
	}
	pub, _ := rt.comments.list(ctx, user, "gallery", "1", "", 10, 0)
	if len(pub) != 2 {
		t.Fatalf("public list after restore = %d, want 2", len(pub))
	}
	if c := countsOf(t, rt, ref("gallery", "1")); c.CommentCount != 2 {
		t.Fatalf("comment_count after restore = %d, want 2", c.CommentCount)
	}
	if err := rt.comments.restore(ctx, top.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore live: want ErrNotFound, got %v", err)
	}
	req := httptest.NewRequest("GET", "/comments/admin", nil)
	req = req.WithContext(withActor(req.Context(), user))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin admin list = %d, want 403", rec.Code)
	}
}
