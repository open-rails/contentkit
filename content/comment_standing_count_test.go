package content

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
)

func TestCommentStandingCountMatchesThread(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	actor := access.Actor{ID: "author"}
	var first Comment
	for i := range 23 {
		cm := mustComment(t, rt, actor, "gallery", cid(1), CommentInput{Body: fmt.Sprintf("root %d", i), Language: "en"})
		if i == 0 {
			first = cm
		}
	}
	list, err := rt.comments.list(context.Background(), actor, rt.Ref("gallery", cid(1)), "en", "", 20, 0)
	if err != nil || len(list) != 20 {
		t.Fatalf("first page: %d %v", len(list), err)
	}
	res := rt.resolver.(*fakeResolver)
	before := res.calls
	rec := doJSON(t, rt.Handler(), actor, "GET", gallery(1, "/can-comment?language=en"), nil)
	var standing struct {
		Count *int `json:"count"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &standing) != nil || standing.Count == nil || *standing.Count != 23 {
		t.Fatalf("authoritative count after first page: %d %s", rec.Code, rec.Body.String())
	}
	if res.calls-before != 1 {
		t.Fatal("count resolved target again")
	}
	for range 3 {
		mustComment(t, rt, actor, "gallery", cid(1), CommentInput{Body: "Japanese root", Language: "ja"})
	}
	mustComment(t, rt, actor, "gallery", cid(1), CommentInput{Body: "Unlabelled root"})
	mustComment(t, rt, actor, "gallery", cid(1), CommentInput{Body: "Reply", ReplyToID: first.ID})
	assertCount := func(language string, want int) {
		t.Helper()
		out := readCommentStanding(t, rt.Handler(), actor, gallery(1, "/can-comment?language="+language))
		all, err := rt.comments.list(context.Background(), actor, rt.Ref("gallery", cid(1)), language, "", 100, 0)
		if err != nil || out.Count == nil || *out.Count != want || *out.Count != len(all) {
			t.Fatalf("%s: count=%v list=%d want=%d err=%v", language, out.Count, len(all), want, err)
		}
	}
	assertCount("en", 23)
	assertCount("ja", 3)
	assertCount("", 27)
	assertCount("unknown", 0)
	if err := rt.comments.softDelete(context.Background(), actor, first.ID); err != nil {
		t.Fatal(err)
	}
	assertCount("en", 23) // the tombstone remains a top-level row
	if err := rt.comments.restore(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	assertCount("en", 23)
}

func readCommentStanding(t *testing.T, h http.Handler, actor access.Actor, path string) CommentStanding {
	t.Helper()
	rec := doJSON(t, h, actor, "GET", path, nil)
	var out CommentStanding
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("standing %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return out
}

func TestCommentStandingCountOwnModerationVersionsAndCanonicalGroups(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	res.setCanonical("tag", cid(2), cid(3))
	rt, pool := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("gallery", "tag")})
	author := access.Actor{ID: "author"}
	mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "Published", Language: "en"})
	for _, state := range []string{ModerationHeld, ModerationRejected} {
		cm := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: state, Language: "en"})
		if _, err := pool.Exec(context.Background(), `UPDATE `+rt.store.t.comments+` SET moderation=$2 WHERE id=$1`, cm.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		actor access.Actor
		want  int
	}{{author, 3}, {access.Actor{ID: "other"}, 1}, {access.Actor{Anonymous: true}, 1}} {
		out := readCommentStanding(t, rt.Handler(), tc.actor, gallery(1, "/can-comment?language=en"))
		if out.Count == nil || *out.Count != tc.want {
			t.Fatalf("%s count %v want %d", tc.actor.ID, out.Count, tc.want)
		}
	}
	for _, version := range []string{"edition-a", "edition-b"} {
		if _, err := rt.comments.create(context.Background(), author, rt.Ref("gallery", cid(1)).WithVersion(version), CommentInput{Body: version, Language: "en"}); err != nil {
			t.Fatal(err)
		}
		out := readCommentStanding(t, rt.Handler(), author, gallery(1, "/can-comment?version="+version+"&language=en"))
		if out.Count == nil || *out.Count != 1 {
			t.Fatalf("version %s count %v", version, out.Count)
		}
	}
	mustComment(t, rt, author, "tag", cid(3), CommentInput{Body: "Canonical group", Language: "en"})
	before := res.calls
	out := readCommentStanding(t, rt.Handler(), author, "/entities/"+cid(2)+"/can-comment?language=en")
	if out.Count == nil || *out.Count != 1 || res.calls-before != 1 {
		t.Fatalf("canonical group count=%v resolver calls=%d", out.Count, res.calls-before)
	}
}

func TestCommentStandingCountWithheldButClosedAndBannedRemainReadable(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	res.set("gallery", cid(2), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res})
	author := access.Actor{ID: "author"}
	mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "Existing"})
	if out := readCommentStanding(t, rt.Handler(), author, gallery(2, "/can-comment")); out.Count == nil || *out.Count != 0 {
		t.Fatalf("empty count: %+v", out)
	}
	res.put("gallery", cid(1), resolve.Resolution{Visible: true, Accessible: true, Comments: CommentsClosed})
	if out := readCommentStanding(t, rt.Handler(), author, gallery(1, "/can-comment")); out.CanComment || out.Count == nil || *out.Count != 1 {
		t.Fatalf("closed comments: %+v", out)
	}
	res.set("gallery", cid(1), true, true)
	if rec := doJSON(t, rt.Handler(), access.Actor{ID: "moderator"}, "PUT", "/global-comment-bans/author", BanInput{}); rec.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}
	if out := readCommentStanding(t, rt.Handler(), author, gallery(1, "/can-comment")); out.CanComment || out.Ban == nil || out.Count == nil || *out.Count != 1 {
		t.Fatalf("banned reader: %+v", out)
	}
	for _, r := range []resolve.Resolution{{Visible: true}, {Visible: true, Accessible: true, Comments: CommentsHidden}} {
		res.put("gallery", cid(1), r)
		rec := doJSON(t, rt.Handler(), author, "GET", gallery(1, "/can-comment"), nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"count"`) {
			t.Fatalf("withheld count: %d %s", rec.Code, rec.Body.String())
		}
	}
	res.set("gallery", cid(1), false, false)
	if rec := doJSON(t, rt.Handler(), author, "GET", gallery(1, "/can-comment"), nil); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), `"count"`) {
		t.Fatalf("invisible target: %d %s", rec.Code, rec.Body.String())
	}
}
