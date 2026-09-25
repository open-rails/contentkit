package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// fakeModerator decides by keyword: "spam" rejects, "iffy" asks for review,
// anything else approves. fail makes every call error; it records its inputs.
type fakeModerator struct {
	mu     sync.Mutex
	fail   bool
	odd    bool // return an unknown decision
	inputs []ModerationInput
}

func (m *fakeModerator) Screen(_ context.Context, in ModerationInput) (Verdict, error) {
	m.mu.Lock()
	m.inputs = append(m.inputs, in)
	fail, odd := m.fail, m.odd
	m.mu.Unlock()
	if fail {
		return Verdict{}, errors.New("model provider down")
	}
	if odd {
		return Verdict{Decision: "maybe", Model: "fake-llm"}, nil
	}
	v := Verdict{Decision: DecisionApprove, Model: "fake-llm", PromptVersion: "v1", Confidence: 0.9}
	text := strings.ToLower(in.Title + " " + in.Text)
	switch {
	case strings.Contains(text, "spam"):
		v.Decision, v.Reason = DecisionReject, "spam is not allowed"
	case strings.Contains(text, "iffy"):
		v.Decision, v.Reason, v.Confidence = DecisionReview, "needs a look", 0.4
	}
	return v, nil
}

func (m *fakeModerator) last() ModerationInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inputs[len(m.inputs)-1]
}

// reviewerOnly grants every perm to "reviewer" only.
type reviewerOnly struct{}

func (reviewerOnly) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "reviewer", nil
}

const reviewPerm = "moderation:review"

func moderatedRuntime(t *testing.T, mod ContentModerator) *Runtime {
	t.Helper()
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}, Moderator: mod,
		Authz: reviewerOnly{}, Perms: Perms{ModerationReview: reviewPerm, CommentModerate: "comment:moderate", PostWrite: postPerm}})
	return rt
}

func listIDs(t *testing.T, rt *Runtime, actor access.Actor) []string {
	t.Helper()
	list, err := rt.comments.list(context.Background(), actor, "gallery", cid(1), "", 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return commentIDs(list)
}

func heldIDs(t *testing.T, rt *Runtime, kind string) []string {
	t.Helper()
	page, err := rt.ListHeld(context.Background(), kind, "", 50)
	if err != nil {
		t.Fatalf("ListHeld: %v", err)
	}
	ids := make([]string, len(page.Items))
	for i, it := range page.Items {
		ids[i] = it.ID
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestModeration_WithoutModeratorPublishes(t *testing.T) {
	rt := moderatedRuntime(t, nil)
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	cm := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "iffy spam or not, it publishes"})
	if cm.Moderation != "" || countsOf(t, rt, ref("gallery", cid(1))).CommentCount != 1 {
		t.Fatalf("without a moderator the comment must publish: %+v", cm)
	}
	if ids := listIDs(t, rt, access.Actor{ID: "other"}); !contains(ids, cm.ID) {
		t.Fatal("published comment missing from a reader's list")
	}
	if ids := heldIDs(t, rt, KindComment); len(ids) != 0 {
		t.Fatalf("held queue = %v, want empty", ids)
	}
	rec := doJSON(t, rt.Handler(), access.Actor{ID: "reviewer"}, "POST", "/posts", postWriteReq{Title: ptr("iffy"), Body: ptr("spam"), IsDraft: ptr(false)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post without a moderator: %d %s", rec.Code, rec.Body.String())
	}
	_ = ctx
}

func TestModeration_CommentVerdicts(t *testing.T) {
	mod := &fakeModerator{}
	rt := moderatedRuntime(t, mod)
	ctx := context.Background()
	h := rt.Handler()
	author, other, anon := access.Actor{ID: "author"}, access.Actor{ID: "other"}, access.Actor{Anonymous: true, IP: "9.9.9.9"}
	g1 := ref("gallery", cid(1))

	// approve
	ok := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "<b>fine</b> text"})
	if ok.Moderation != "" || countsOf(t, rt, g1).CommentCount != 1 {
		t.Fatalf("approved comment = %+v", ok)
	}
	in := mod.last()
	if in.Tenant != testTenant || in.Kind != KindComment || in.Text != "fine text" || !in.Ref.Equal(g1) || in.ItemID != "" || in.Actor.ID != "author" {
		t.Fatalf("moderator input = %+v, want sanitized text and the content reference", in)
	}

	// reject: 422 with the reason, nothing stored
	rec := doJSON(t, h, author, "POST", "/gallery/"+cid(1)+"/comments", createInput{Body: "buy spam here"})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "spam is not allowed") {
		t.Fatalf("reject = %d %s, want 422 with the reason", rec.Code, rec.Body.String())
	}
	var rej RejectedError
	if _, err := rt.comments.create(ctx, author, "gallery", cid(1), createInput{Body: "spam"}); !errors.As(err, &rej) || rej.Reason != "spam is not allowed" {
		t.Fatalf("reject error = %v, want RejectedError", err)
	}
	if n := len(listIDs(t, rt, author)); n != 1 {
		t.Fatalf("rejected comment was stored: %d comments", n)
	}

	// review: stored held, 202, author-only
	rec = doJSON(t, h, author, "POST", "/gallery/"+cid(1)+"/comments", createInput{Body: "iffy remark"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("review = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var held Comment
	if err := json.Unmarshal(rec.Body.Bytes(), &held); err != nil || held.Moderation != ModerationHeld || held.ModerationReason != "needs a look" {
		t.Fatalf("held response = %+v err=%v", held, err)
	}
	if c := countsOf(t, rt, g1).CommentCount; c != 1 {
		t.Fatalf("held comment was counted: %d", c)
	}
	if contains(listIDs(t, rt, other), held.ID) || contains(listIDs(t, rt, anon), held.ID) {
		t.Fatal("held comment visible to another reader")
	}
	mine, _ := rt.comments.list(ctx, author, "gallery", cid(1), "", 50, 0)
	i := indexOfComment(mine, held.ID)
	if i < 0 || mine[i].Moderation != ModerationHeld || mine[i].ModerationReason != "needs a look" || mine[i].Body != "iffy remark" {
		t.Fatalf("author's view of the held comment = %+v", mine)
	}
	// A held comment cannot be reacted to, and never credits its author.
	if _, err := rt.comments.reactTx(ctx, other, ok.ID, 1); err != nil {
		t.Fatalf("react to the published comment: %v", err)
	}
	if _, err := rt.comments.reactTx(ctx, other, held.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reacted to a held comment: %v", err)
	}
	if m, _ := rt.CommentReactionsByAuthor(ctx, []string{"author"}); m["author"] != (AuthorReactions{Likes: 1}) {
		t.Fatalf("author totals = %+v, want only the published comment's like", m)
	}
	if feed, _ := rt.comments.latest(ctx, author, 50, 0); len(feed) != 1 || feed[0].ID != held.ID {
		if len(feed) != 1 {
			t.Fatalf("feed = %v, want only the approved comment", commentFeedIDs(feed))
		}
	}
	if _, err := rt.comments.create(ctx, other, "gallery", cid(1), createInput{Body: "reply", ReplyToID: held.ID}); err == nil {
		t.Fatal("a reply to a held comment was accepted")
	}
	if _, err := rt.comments.reactTx(ctx, other, held.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reaction on a held comment: %v, want ErrNotFound", err)
	}
	if reps, _ := rt.comments.replies(ctx, other, held.ID, 10, 0); len(reps) != 0 {
		t.Fatalf("replies of a held comment = %v", reps)
	}

	// review queue
	page, err := rt.ListHeld(ctx, KindComment, "", 10)
	if err != nil || len(page.Items) != 1 || page.Next != "" {
		t.Fatalf("ListHeld = %+v err=%v", page, err)
	}
	it := page.Items[0]
	if it.ID != held.ID || it.Kind != KindComment || !it.Ref.Equal(g1) || it.AuthorID != "author" || it.Body != "iffy remark" ||
		it.Reason != "needs a look" || it.Model != "fake-llm" || it.PromptVersion != "v1" || it.Confidence != 0.4 || it.Error != "" || it.HeldAt.IsZero() {
		t.Fatalf("held item = %+v", it)
	}

	// resolve approve: published and counted
	if err := rt.Resolve(ctx, KindComment, held.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "reviewer"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !contains(listIDs(t, rt, other), held.ID) || countsOf(t, rt, g1).CommentCount != 2 {
		t.Fatal("approved comment not published/counted")
	}
	mine, _ = rt.comments.list(ctx, author, "gallery", cid(1), "", 50, 0)
	if i = indexOfComment(mine, held.ID); mine[i].Moderation != "" || mine[i].ModerationReason != "" {
		t.Fatalf("approved comment still carries moderation state: %+v", mine[i])
	}
	if ids := heldIDs(t, rt, KindComment); len(ids) != 0 {
		t.Fatalf("queue after approve = %v", ids)
	}
	if err := rt.Resolve(ctx, KindComment, held.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "reviewer"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-resolve: %v, want ErrNotFound", err)
	}

	// resolve reject: final, author-visible with the reviewer's reason
	rej2 := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "another iffy one"})
	if err := rt.Resolve(ctx, KindComment, rej2.ID, ReviewDecision{Revision: 1, Decision: DecisionReject, Reviewer: "reviewer", Reason: "off topic"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if contains(listIDs(t, rt, other), rej2.ID) || countsOf(t, rt, g1).CommentCount != 2 {
		t.Fatal("rejected comment leaked or was counted")
	}
	mine, _ = rt.comments.list(ctx, author, "gallery", cid(1), "", 50, 0)
	if i = indexOfComment(mine, rej2.ID); i < 0 || mine[i].Moderation != ModerationRejected || mine[i].ModerationReason != "off topic" {
		t.Fatalf("author's view of the rejected comment = %+v", mine)
	}
	if ids := heldIDs(t, rt, KindComment); len(ids) != 0 {
		t.Fatalf("queue after reject = %v", ids)
	}
	// admin list shows everything with its state
	adm, _ := rt.comments.adminList(ctx, "", 50, 0)
	states := map[string]string{}
	for _, a := range adm {
		states[a.ID] = a.Moderation
	}
	if states[rej2.ID] != ModerationRejected || states[held.ID] != ModerationApproved {
		t.Fatalf("admin states = %v", states)
	}

	// validation
	if err := rt.Resolve(ctx, KindComment, rej2.ID, ReviewDecision{Revision: 1, Decision: DecisionReview, Reviewer: "r"}); err == nil {
		t.Fatal("review is not a resolution")
	}
	if err := rt.Resolve(ctx, "gallery", rej2.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "r"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := rt.Resolve(ctx, KindComment, rej2.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove}); err == nil {
		t.Fatal("missing reviewer accepted")
	}
	if _, err := rt.ListHeld(ctx, "gallery", "", 10); err == nil {
		t.Fatal("ListHeld accepted an unknown kind")
	}
}

func TestModeration_ListHeldPages(t *testing.T) {
	rt := moderatedRuntime(t, &fakeModerator{})
	ctx := context.Background()
	var want []string
	for i := 0; i < 5; i++ {
		want = append(want, mustComment(t, rt, access.Actor{ID: "a"}, "gallery", cid(1), createInput{Body: fmt.Sprintf("iffy %d", i)}).ID)
	}
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := rt.ListHeld(ctx, KindComment, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			got = append(got, it.ID)
		}
		if page.Next == "" {
			if pages != 2 {
				t.Fatalf("pages = %d, want 3", pages+1)
			}
			break
		}
		cursor = page.Next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged queue = %v, want oldest-first %v", got, want)
	}
	if _, err := rt.ListHeld(ctx, KindComment, "garbage", 2); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

func TestModeration_FailClosed(t *testing.T) {
	mod := &fakeModerator{fail: true}
	rt := moderatedRuntime(t, mod)
	ctx := context.Background()
	author, other := access.Actor{ID: "author"}, access.Actor{ID: "other"}

	cm := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "perfectly fine"})
	if cm.Moderation != ModerationHeld || cm.ModerationReason != heldReason {
		t.Fatalf("moderator error must hold: %+v", cm)
	}
	if contains(listIDs(t, rt, other), cm.ID) || countsOf(t, rt, ref("gallery", cid(1))).CommentCount != 0 {
		t.Fatal("unscreened comment was published")
	}
	page, _ := rt.ListHeld(ctx, KindComment, "", 10)
	if len(page.Items) != 1 || page.Items[0].Error != "moderator unavailable" || page.Items[0].Model != "" {
		t.Fatalf("held item after failure = %+v", page.Items)
	}
	if err := rt.Resolve(ctx, KindComment, cm.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if !contains(listIDs(t, rt, other), cm.ID) {
		t.Fatal("approved comment not published")
	}

	// An unknown decision is not a publish either.
	mod.fail, mod.odd = false, true
	odd := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "fine too"})
	if odd.Moderation != ModerationHeld || odd.ModerationReason != heldReason {
		t.Fatalf("unknown decision must hold: %+v", odd)
	}
	// Posts fail closed the same way.
	mod.odd, mod.fail = false, true
	rec := doJSON(t, rt.Handler(), access.Actor{ID: "reviewer"}, "POST", "/posts", postWriteReq{Title: ptr("t"), Body: ptr("b"), IsDraft: ptr(false)})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"moderation":"held"`) {
		t.Fatalf("post under a failing moderator = %d %s, want 202 held", rec.Code, rec.Body.String())
	}
}

func TestModeration_EditRescreens(t *testing.T) {
	rt := moderatedRuntime(t, &fakeModerator{})
	ctx := context.Background()
	author, other := access.Actor{ID: "author"}, access.Actor{ID: "other"}
	g1 := ref("gallery", cid(1))

	top := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "fine"})
	reply := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "fine reply", ReplyToID: top.ID})
	if c := countsOf(t, rt, g1).CommentCount; c != 1 {
		t.Fatalf("comment_count = %d", c)
	}
	// published -> held on edit: withdrawn and uncounted
	ed, err := rt.comments.edit(ctx, author, top.ID, "now iffy")
	if err != nil || ed.Moderation != ModerationHeld || ed.Body != "now iffy" {
		t.Fatalf("edit to review = %+v err=%v", ed, err)
	}
	if contains(listIDs(t, rt, other), top.ID) || countsOf(t, rt, g1).CommentCount != 0 {
		t.Fatal("withdrawn comment still visible or counted")
	}
	// held -> published on a fixed edit
	if ed, err = rt.comments.edit(ctx, author, top.ID, "fine again"); err != nil || ed.Moderation != "" {
		t.Fatalf("edit back = %+v err=%v", ed, err)
	}
	if !contains(listIDs(t, rt, other), top.ID) || countsOf(t, rt, g1).CommentCount != 1 {
		t.Fatal("re-approved comment not published/counted")
	}
	// reject on edit changes nothing
	if _, err := rt.comments.edit(ctx, author, top.ID, "spam"); err == nil {
		t.Fatal("edit to spam accepted")
	}
	list, _ := rt.comments.list(ctx, other, "gallery", cid(1), "", 50, 0)
	if list[indexOfComment(list, top.ID)].Body != "fine again" {
		t.Fatal("rejected edit changed the body")
	}
	// replies adjust reply_count
	if _, err := rt.comments.edit(ctx, author, reply.ID, "iffy reply"); err != nil {
		t.Fatal(err)
	}
	list, _ = rt.comments.list(ctx, other, "gallery", cid(1), "", 50, 0)
	if rc := list[indexOfComment(list, top.ID)].ReplyCount; rc != 0 {
		t.Fatalf("reply_count after withdrawing the reply = %d", rc)
	}
	if reps, _ := rt.comments.replies(ctx, other, top.ID, 10, 0); len(reps) != 0 {
		t.Fatal("held reply visible to a reader")
	}
	if reps, _ := rt.comments.replies(ctx, author, top.ID, 10, 0); len(reps) != 1 || reps[0].Moderation != ModerationHeld {
		t.Fatalf("author's replies = %+v", reps)
	}
	if err := rt.Resolve(ctx, KindComment, reply.ID, ReviewDecision{Revision: 2, Decision: DecisionApprove, Reviewer: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	list, _ = rt.comments.list(ctx, other, "gallery", cid(1), "", 50, 0)
	if rc := list[indexOfComment(list, top.ID)].ReplyCount; rc != 1 {
		t.Fatalf("reply_count after approving the reply = %d", rc)
	}
	// deleting/restoring a held comment never touches counts
	held := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "iffy"})
	if err := rt.comments.softDelete(ctx, author, held.ID); err != nil {
		t.Fatal(err)
	}
	if err := rt.comments.restore(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	if c := countsOf(t, rt, g1).CommentCount; c != 1 {
		t.Fatalf("comment_count after held delete/restore = %d, want 1", c)
	}
	if contains(listIDs(t, rt, other), held.ID) {
		t.Fatal("restored held comment published")
	}
}

func TestModeration_Posts(t *testing.T) {
	mod := &fakeModerator{}
	rt := moderatedRuntime(t, mod)
	ctx := context.Background()
	h := rt.Handler()
	editor, reader := access.Actor{ID: "reviewer"}, access.Actor{ID: "reader"}

	rec := doJSON(t, h, editor, "POST", "/posts", postWriteReq{Title: ptr("Hello"), Body: ptr("an iffy body"), Language: ptr("en"), IsDraft: ptr(false)})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("held post = %d %s, want 202", rec.Code, rec.Body.String())
	}
	held := decodePost(t, rec)
	if held.Moderation != ModerationHeld || held.ModerationReason != "needs a look" {
		t.Fatalf("held post = %+v", held)
	}
	if in := mod.last(); in.Kind != KindPost || in.Title != "Hello" || in.Text != "an iffy body" || in.Ref.ContentID != held.ID || in.ItemID != held.ID {
		t.Fatalf("moderator input = %+v", in)
	}
	if n := len(listPosts(t, h, "")); n != 0 {
		t.Fatalf("held post listed publicly (%d)", n)
	}
	if rec = doJSON(t, h, reader, "GET", "/posts/"+held.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("held post to a reader = %d, want 404", rec.Code)
	}
	if rec = doJSON(t, h, editor, "GET", "/posts/"+held.ID, nil); rec.Code != http.StatusOK || decodePost(t, rec).ModerationReason != "needs a look" {
		t.Fatalf("held post to its author = %d %s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, h, reader, "POST", "/posts/"+held.ID+"/like", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("like on a held post = %d, want 404", rec.Code)
	}
	docs, err := rt.KeywordDocuments(ctx, testTenant, KindPost, "en", []contentref.ContentRef{rt.Ref(KindPost, held.ID)})
	if err != nil || len(docs) != 0 {
		t.Fatalf("held post produced a keyword document: %v err=%v", docs, err)
	}
	if refs, _, _, _ := rt.ListContent(ctx, testTenant, KindPost, "en", "", 10); len(refs) != 0 {
		t.Fatalf("held post listed for indexing: %v", refs)
	}
	page, _ := rt.ListHeld(ctx, KindPost, "", 10)
	if len(page.Items) != 1 || page.Items[0].ID != held.ID || page.Items[0].Title != "Hello" || page.Items[0].AuthorID != "reviewer" || page.Items[0].Ref.ContentKind != KindPost {
		t.Fatalf("held posts = %+v", page.Items)
	}
	if err := rt.Resolve(ctx, KindPost, held.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if n := len(listPosts(t, h, "")); n != 1 {
		t.Fatalf("approved post not listed (%d)", n)
	}
	if docs, _ = rt.KeywordDocuments(ctx, testTenant, KindPost, "en", []contentref.ContentRef{rt.Ref(KindPost, held.ID)}); len(docs) != 1 || docs[0].Title != "Hello" {
		t.Fatalf("approved post keyword document = %v", docs)
	}
	if rec = doJSON(t, h, reader, "POST", "/posts/"+held.ID+"/like", nil); rec.Code != http.StatusOK {
		t.Fatalf("like on the approved post = %d", rec.Code)
	}

	// reject on create: 422, nothing stored
	if rec = doJSON(t, h, editor, "POST", "/posts", postWriteReq{Title: ptr("spam"), Body: ptr("b"), IsDraft: ptr(false)}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rejected post = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(listPosts(t, h, "")); n != 1 {
		t.Fatalf("rejected post stored (%d)", n)
	}

	// drafts are not screened; publishing one is
	calls := len(mod.inputs)
	rec = doJSON(t, h, editor, "POST", "/posts", postWriteReq{Title: ptr("Draft"), Body: ptr("iffy draft"), IsDraft: ptr(true)})
	if rec.Code != http.StatusCreated || len(mod.inputs) != calls {
		t.Fatalf("draft = %d, moderator calls %d -> %d", rec.Code, calls, len(mod.inputs))
	}
	draft := decodePost(t, rec)
	if rec = doJSON(t, h, editor, "PATCH", "/posts/"+draft.ID, postWriteReq{IsDraft: ptr(false)}); rec.Code != http.StatusAccepted || decodePost(t, rec).Moderation != ModerationHeld {
		t.Fatalf("publishing an iffy draft = %d %s, want held", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, h, editor, "PATCH", "/posts/"+draft.ID, postWriteReq{Body: ptr("clean now")}); rec.Code != http.StatusOK || decodePost(t, rec).Moderation != "" {
		t.Fatalf("fixed post = %d %s, want approved", rec.Code, rec.Body.String())
	}
	if n := len(listPosts(t, h, "")); n != 2 {
		t.Fatalf("published posts = %d, want 2", n)
	}
	if rec = doJSON(t, h, editor, "PATCH", "/posts/"+draft.ID, postWriteReq{Title: ptr("spam title")}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rejected edit = %d", rec.Code)
	}
	if rec = doJSON(t, h, editor, "GET", "/posts/"+draft.ID, nil); decodePost(t, rec).Title != "Draft" {
		t.Fatal("rejected edit changed the title")
	}
}

func TestModeration_BasicModeratorAndChain(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	basic := &BasicModerator{now: func() time.Time { return now }}
	ai := &fakeModerator{}
	rt := moderatedRuntime(t, Chain{basic, ai})
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	rejects := func(body, want string) {
		t.Helper()
		var rej RejectedError
		if _, err := rt.comments.create(ctx, author, "gallery", cid(1), createInput{Body: body}); !errors.As(err, &rej) || rej.Reason != want {
			t.Fatalf("%q: %v, want reject %q", body, err, want)
		}
	}
	rejects("see https://example.com/x", "links are not allowed")
	rejects("visit www.example.com now", "links are not allowed")
	rejects("cp trade", "content violates policy")
	first := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "hello there"})
	rejects("hello there", "duplicate submission, slow down")
	if _, err := rt.comments.edit(ctx, author, first.ID, "hello there"); err != nil { // edits skip the dup guard
		t.Fatalf("edit with the same text: %v", err)
	}
	if _, err := rt.comments.create(ctx, access.Actor{ID: "someone-else"}, "gallery", cid(1), createInput{Body: "hello there"}); err != nil {
		t.Fatalf("another actor's identical text: %v", err)
	}
	now = now.Add(31 * time.Second)
	if _, err := rt.comments.create(ctx, author, "gallery", cid(1), createInput{Body: "hello there"}); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	// the AI moderator behind it still holds
	if cm := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "iffy"}); cm.Moderation != ModerationHeld {
		t.Fatalf("chain did not reach the second moderator: %+v", cm)
	}
	if in := ai.last(); in.Text != "iffy" {
		t.Fatalf("second moderator saw %+v", in)
	}
	calls := len(ai.inputs)
	rejects("spam www.x.com", "links are not allowed") // basic short-circuits
	if len(ai.inputs) != calls {
		t.Fatal("the chain called the AI moderator after a basic reject")
	}
	// options
	lenient := &BasicModerator{AllowLinks: true, DupWindow: -1, CensorWords: []string{}}
	v, _ := lenient.Screen(ctx, ModerationInput{Actor: author, Text: "cp https://x.y"})
	if v.Decision != DecisionApprove {
		t.Fatalf("lenient basic = %+v", v)
	}
	v, _ = lenient.Screen(ctx, ModerationInput{Actor: author, Text: "cp https://x.y"})
	if v.Decision != DecisionApprove {
		t.Fatalf("lenient basic (dup disabled) = %+v", v)
	}
}

func TestModeration_ReviewRoutes(t *testing.T) {
	rt := moderatedRuntime(t, &fakeModerator{})
	h := rt.Handler()
	author, reviewer, user := access.Actor{ID: "author"}, access.Actor{ID: "reviewer"}, access.Actor{ID: "user"}
	held := mustComment(t, rt, author, "gallery", cid(1), createInput{Body: "iffy"})

	if rec := doJSON(t, h, user, "GET", "/moderation/held?kind=comment", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("queue without the perm = %d", rec.Code)
	}
	rec := doJSON(t, h, reviewer, "GET", "/moderation/held?kind=comment&limit=10", nil)
	var page HeldPage
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != held.ID || page.Items[0].Ref.ContentID != cid(1) {
		t.Fatalf("queue = %d %s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, h, reviewer, "GET", "/moderation/held?kind=gallery", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("queue with a bad kind = %d", rec.Code)
	}
	if rec = doJSON(t, h, user, "POST", "/moderation/comment/"+held.ID+"/resolve", map[string]any{"revision": int64(1), "decision": "approve"}); rec.Code != http.StatusForbidden {
		t.Fatalf("resolve without the perm = %d", rec.Code)
	}
	if rec = doJSON(t, h, reviewer, "POST", "/moderation/comment/"+held.ID+"/resolve", map[string]any{"revision": int64(1), "decision": "review"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("resolve with a bad decision = %d", rec.Code)
	}
	if rec = doJSON(t, h, reviewer, "POST", "/moderation/comment/"+held.ID+"/resolve", map[string]any{"revision": int64(1), "decision": "reject", "reason": "nope"}); rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, h, reviewer, "POST", "/moderation/comment/"+held.ID+"/resolve", map[string]any{"revision": int64(1), "decision": "approve"}); rec.Code != http.StatusNotFound {
		t.Fatalf("re-resolve = %d, want 404", rec.Code)
	}
	if rec = doJSON(t, h, reviewer, "POST", "/moderation/post/nope/resolve", map[string]any{"revision": int64(1), "decision": "approve"}); rec.Code != http.StatusNotFound {
		t.Fatalf("resolve a missing post = %d, want 404", rec.Code)
	}
	list, _ := rt.comments.list(context.Background(), author, "gallery", cid(1), "", 10, 0)
	if list[0].Moderation != ModerationRejected || list[0].ModerationReason != "nope" {
		t.Fatalf("author's view after the route reject = %+v", list[0])
	}
	// the reviewer's identity is recorded
	var by string
	if err := rt.store.pool.QueryRow(context.Background(), `SELECT moderated_by FROM `+rt.store.t.comments+` WHERE id = $1`, held.ID).Scan(&by); err != nil || by != "reviewer" {
		t.Fatalf("moderated_by = %q err=%v", by, err)
	}
}

func TestModeration_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	mod := &fakeModerator{}
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Resolver: res, ContentKinds: []string{"gallery"}, Moderator: mod, Perms: Perms{PostWrite: postPerm}})
	b, err := New(ctx, Options{Pool: pool, Schema: a.schema, Tenant: "site_b", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: res, ContentKinds: []string{"gallery"}, Moderator: mod, Perms: Perms{PostWrite: postPerm}})
	if err != nil {
		t.Fatal(err)
	}
	author := access.Actor{ID: "shared-account"}
	held := mustComment(t, a, author, "gallery", cid(1), createInput{Body: "iffy"})
	if in := mod.last(); in.Tenant != "site_a" {
		t.Fatalf("moderator saw tenant %q", in.Tenant)
	}
	rec := doJSON(t, a.Handler(), author, "POST", "/posts", postWriteReq{Title: ptr("t"), Body: ptr("iffy"), IsDraft: ptr(false)})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("held post = %d", rec.Code)
	}
	post := decodePost(t, rec)

	if ids := heldIDs(t, b, KindComment); len(ids) != 0 {
		t.Fatalf("tenant b sees a's held comments: %v", ids)
	}
	if ids := heldIDs(t, b, KindPost); len(ids) != 0 {
		t.Fatalf("tenant b sees a's held posts: %v", ids)
	}
	if err := b.Resolve(ctx, KindComment, held.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "r"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b resolved a's comment: %v", err)
	}
	if err := b.Resolve(ctx, KindPost, post.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "r"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b resolved a's post: %v", err)
	}
	if list, _ := b.comments.list(ctx, author, "gallery", cid(1), "", 10, 0); len(list) != 0 {
		t.Fatalf("the shared account sees a's held comment through tenant b: %v", commentIDs(list))
	}
	if ids := heldIDs(t, a, KindComment); len(ids) != 1 || ids[0] != held.ID {
		t.Fatalf("tenant a's queue = %v", ids)
	}
}

func (*fakeModerator) StatelessPolicy() {}
