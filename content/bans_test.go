package content

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

const banPerm = "comments:ban"

// banAuthz grants Perms.CommentBan to operators only, every other perm to all.
type banAuthz struct{ operators map[string]bool }

func (a banAuthz) Can(_ context.Context, actor access.Actor, perm string) (bool, error) {
	return perm != banPerm || a.operators[actor.ID], nil
}

var (
	owner    = access.Actor{ID: "owner", Kind: "user"}
	other    = access.Actor{ID: "other", Kind: "user"}
	troll    = access.Actor{ID: "troll", Kind: "user"}
	fan      = access.Actor{ID: "fan", Kind: "user"}
	operator = access.Actor{ID: "op", Kind: "user"}
)

// bansRuntime: gallery 1 is owner's, 2 other's, 3 troll's own, 4 nobody's.
func bansRuntime(t *testing.T) (*Runtime, http.Handler) {
	res := &fakeResolver{}
	res.setOwned("gallery", cid(1), owner.ID)
	res.setOwned("gallery", cid(2), other.ID)
	res.setOwned("gallery", cid(3), troll.ID)
	res.setOwned("gallery", cid(4), "")
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}, Users: commentsEnricher{},
		Authz: banAuthz{operators: map[string]bool{operator.ID: true}}, Perms: Perms{CommentBan: banPerm, CommentModerate: "mod"}})
	return rt, rt.Handler()
}

func gallery(n int, rest string) string { return "/gallery/" + cid(n) + rest }

// comment posts a comment (a reply when replyTo is set) and returns the response.
func comment(t *testing.T, h http.Handler, who access.Actor, n int, replyTo string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, who, "POST", gallery(n, "/comments"), createInput{Body: "words", ReplyToID: replyTo})
}

func created(t *testing.T, rec *httptest.ResponseRecorder) Comment {
	t.Helper()
	var c Comment
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &c) != nil {
		t.Fatalf("want 201, got %d %s", rec.Code, rec.Body.String())
	}
	return c
}

// assertBanned checks a 403 comment_banned naming scope; until reports whether
// the notice carries an end.
func assertBanned(t *testing.T, rec *httptest.ResponseRecorder, scope string, until bool) {
	t.Helper()
	got := decodeErr(t, rec.Body.String())
	if rec.Code != http.StatusForbidden || got.Code != CodeCommentBanned || got.Ban == nil || got.Ban.Scope != scope || (got.Ban.Until != nil) != until {
		t.Fatalf("want 403 comment_banned in %s (until %v), got %d %s", scope, until, rec.Code, rec.Body.String())
	}
}

func bans(t *testing.T, h http.Handler, who access.Actor, path string) []CommentBan {
	t.Helper()
	rec := doJSON(t, h, who, "GET", path, nil)
	var out []CommentBan
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return out
}

func standing(t *testing.T, h http.Handler, who access.Actor, n int) canComment {
	t.Helper()
	rec := doJSON(t, h, who, "GET", gallery(n, "/can-comment"), nil)
	var out canComment
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("can-comment: %d %s", rec.Code, rec.Body.String())
	}
	return out
}

// An owner's ban stops every new comment of the user on that owner's content
// only (top-level, replies to anyone including themselves, edits); it leaves
// their existing comments visible, their reactions and favorites, and other
// owners' content alone. Owners see and lift only their own scope.
func TestCommentBanOwnerScope(t *testing.T) {
	_, h := bansRuntime(t)
	mine := created(t, comment(t, h, troll, 1, ""))
	theirs := created(t, comment(t, h, fan, 1, ""))
	onOwn := created(t, comment(t, h, troll, 3, ""))

	rec := doJSON(t, h, owner, "PUT", "/comment-bans/"+troll.ID, banInput{Reason: "spam"})
	var b CommentBan
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil || b.Scope != OwnerScope(owner.ID) ||
		b.UserID != troll.ID || b.BannedBy != owner.ID || b.Until != nil || b.Expired || b.User == nil || b.User.Username != "name-troll" {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}

	assertBanned(t, comment(t, h, troll, 1, ""), "owner:owner", false)
	assertBanned(t, comment(t, h, troll, 1, theirs.ID), "owner:owner", false)
	assertBanned(t, comment(t, h, troll, 1, mine.ID), "owner:owner", false)
	assertBanned(t, doJSON(t, h, troll, "PATCH", "/comments/"+mine.ID, editInput{Body: "edited"}), "owner:owner", false)
	if got := decodeErr(t, comment(t, h, troll, 1, "").Body.String()); got.Ban.Reason != "spam" {
		t.Fatalf("the notice's reason: %+v", got.Ban)
	}
	created(t, comment(t, h, troll, 2, ""))
	created(t, comment(t, h, troll, 3, ""))
	created(t, comment(t, h, troll, 3, onOwn.ID))
	for _, s := range []step{{"POST", gallery(1, "/like"), nil}, {"POST", gallery(1, "/favorite"), nil}, {"POST", "/comments/" + theirs.ID + "/like", nil}} {
		if rec := doJSON(t, h, troll, s.method, s.path, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s %s while banned: %d %s", s.method, s.path, rec.Code, rec.Body.String())
		}
	}

	rec = doJSON(t, h, access.Actor{Anonymous: true, IP: "10.0.0.9"}, "GET", gallery(1, "/comments"), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"`+mine.ID+`"`) || strings.Contains(rec.Body.String(), commentTombstone) {
		t.Fatalf("existing comments must stay visible: %d %s", rec.Code, rec.Body.String())
	}

	if s := standing(t, h, troll, 1); s.CanComment || s.Ban == nil || s.Ban.Scope != "owner:owner" || s.Ban.Reason != "spam" {
		t.Fatalf("standing on the owner's content: %+v", s)
	}
	if s := standing(t, h, troll, 2); !s.CanComment || s.Ban != nil {
		t.Fatalf("standing elsewhere: %+v", s)
	}

	if l := bans(t, h, owner, "/comment-bans"); len(l) != 1 || l[0].UserID != troll.ID {
		t.Fatalf("owner's list: %+v", l)
	}
	if l := bans(t, h, other, "/comment-bans"); len(l) != 0 {
		t.Fatalf("another owner sees: %+v", l)
	}
	if l := bans(t, h, operator, "/global-comment-bans"); len(l) != 0 {
		t.Fatalf("the operator sees owner bans: %+v", l)
	}
	if rec := doJSON(t, h, other, "DELETE", "/comment-bans/"+troll.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("lift in one's own (empty) scope: %d", rec.Code)
	}
	assertBanned(t, comment(t, h, troll, 1, ""), "owner:owner", false)

	if rec := doJSON(t, h, owner, "DELETE", "/comment-bans/"+troll.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("lift: %d %s", rec.Code, rec.Body.String())
	}
	created(t, comment(t, h, troll, 1, ""))
	if l := bans(t, h, owner, "/comment-bans"); len(l) != 0 {
		t.Fatalf("after lift: %+v", l)
	}
}

// A global ban stops the user commenting anywhere in the tenant, on their own
// content and replies to their own comments included, and hides nothing.
// Only Perms.CommentBan may set it; of two bans the longer one is shown.
func TestCommentBanGlobalScope(t *testing.T) {
	_, h := bansRuntime(t)
	onOwn := created(t, comment(t, h, troll, 3, ""))

	if rec := doJSON(t, h, owner, "PUT", "/global-comment-bans/"+troll.ID, banInput{}); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-operator's global ban: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, owner, "GET", "/global-comment-bans", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-operator's global list: %d", rec.Code)
	}
	if rec := doJSON(t, h, access.Actor{Anonymous: true, IP: "10.0.0.9"}, "PUT", "/comment-bans/"+troll.ID, banInput{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous ban: %d", rec.Code)
	}
	if rec := doJSON(t, h, operator, "PUT", "/global-comment-bans/"+troll.ID, banInput{Reason: "abuse"}); rec.Code != http.StatusOK {
		t.Fatalf("global ban: %d %s", rec.Code, rec.Body.String())
	}
	for _, n := range []int{1, 2, 3, 4} {
		assertBanned(t, comment(t, h, troll, n, ""), ScopeGlobal, false)
	}
	assertBanned(t, comment(t, h, troll, 3, onOwn.ID), ScopeGlobal, false)
	assertBanned(t, doJSON(t, h, troll, "PATCH", "/comments/"+onOwn.ID, editInput{Body: "edited"}), ScopeGlobal, false)
	if rec := doJSON(t, h, operator, "PATCH", "/comments/"+onOwn.ID, editInput{Body: "moderated"}); rec.Code != http.StatusOK {
		t.Fatalf("a moderator edits a banned user's comment: %d %s", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, h, fan, "GET", gallery(3, "/comments"), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"`+onOwn.ID+`"`) {
		t.Fatalf("existing comments must stay visible: %d %s", rec.Code, rec.Body.String())
	}
	if s := standing(t, h, troll, 3); s.CanComment || s.Ban == nil || s.Ban.Scope != ScopeGlobal {
		t.Fatalf("standing on own content: %+v", s)
	}
	if l := bans(t, h, operator, "/global-comment-bans"); len(l) != 1 || l[0].UserID != troll.ID || l[0].Reason != "abuse" || l[0].BannedBy != operator.ID {
		t.Fatalf("global list: %+v", l)
	}
	if l := bans(t, h, owner, "/comment-bans"); len(l) != 0 {
		t.Fatalf("an owner sees global bans: %+v", l)
	}

	until := time.Now().Add(time.Hour)
	if rec := doJSON(t, h, owner, "PUT", "/comment-bans/"+troll.ID, banInput{Until: &until}); rec.Code != http.StatusOK {
		t.Fatalf("owner ban: %d %s", rec.Code, rec.Body.String())
	}
	assertBanned(t, comment(t, h, troll, 1, ""), ScopeGlobal, false) // the indefinite one
	if rec := doJSON(t, h, operator, "DELETE", "/global-comment-bans/"+troll.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("lift: %d", rec.Code)
	}
	created(t, comment(t, h, troll, 2, ""))
	assertBanned(t, comment(t, h, troll, 1, ""), "owner:owner", true)
}

// A ban with an end stops comments until then; afterwards it stays listed as
// expired until replaced or lifted. Bans validate their input.
func TestCommentBanExpiryAndValidation(t *testing.T) {
	_, h := bansRuntime(t)
	until := time.Now().Add(3 * time.Second)
	if rec := doJSON(t, h, owner, "PUT", "/comment-bans/"+troll.ID, banInput{Reason: "cool off", Until: &until}); rec.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}
	assertBanned(t, comment(t, h, troll, 1, ""), "owner:owner", true)
	if l := bans(t, h, owner, "/comment-bans"); len(l) != 1 || l[0].Expired || l[0].Until == nil {
		t.Fatalf("live ban: %+v", l)
	}
	time.Sleep(time.Until(until) + 100*time.Millisecond)
	created(t, comment(t, h, troll, 1, ""))
	if s := standing(t, h, troll, 1); !s.CanComment {
		t.Fatalf("standing after expiry: %+v", s)
	}
	if l := bans(t, h, owner, "/comment-bans"); len(l) != 1 || !l[0].Expired || l[0].Reason != "cool off" {
		t.Fatalf("expired ban: %+v", l)
	}
	if rec := doJSON(t, h, owner, "PUT", "/comment-bans/"+troll.ID, nil); rec.Code != http.StatusOK { // no body: {}
		t.Fatalf("re-ban: %d %s", rec.Code, rec.Body.String())
	}
	assertBanned(t, comment(t, h, troll, 1, ""), "owner:owner", false)
	if l := bans(t, h, owner, "/comment-bans"); len(l) != 1 || l[0].Expired || l[0].Until != nil || l[0].Reason != "" {
		t.Fatalf("re-ban replaces: %+v", l)
	}

	past := time.Now().Add(-time.Minute)
	for name, c := range map[string]struct {
		path string
		body any
	}{
		"until in the past": {"/comment-bans/" + troll.ID, banInput{Until: &past}},
		"yourself":          {"/comment-bans/" + owner.ID, banInput{}},
		"long reason":       {"/comment-bans/" + troll.ID, banInput{Reason: strings.Repeat("x", maxBanReason+1)}},
		"unknown field":     {"/comment-bans/" + troll.ID, map[string]string{"scope": "global"}},
	} {
		if rec := doJSON(t, h, owner, "PUT", c.path, c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// Erasure removes bans of an erased user and their owner scope's bans, and
// clears them as the operator of global bans that keep standing; an erased
// user cannot be banned again.
func TestCommentBanErasure(t *testing.T) {
	rt, h := bansRuntime(t)
	ctx := context.Background()
	for _, s := range []struct {
		who  access.Actor
		path string
	}{{owner, "/comment-bans/troll"}, {owner, "/comment-bans/fan"}, {operator, "/global-comment-bans/troll"}, {operator, "/global-comment-bans/fan"}} {
		if rec := doJSON(t, h, s.who, "PUT", s.path, banInput{}); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", s.path, rec.Code, rec.Body.String())
		}
	}
	if err := rt.EraseSubjects(ctx, []string{troll.ID, operator.ID}); err != nil {
		t.Fatal(err)
	}
	if l := bans(t, h, owner, "/comment-bans"); len(l) != 1 || l[0].UserID != fan.ID {
		t.Fatalf("owner scope after erasing troll: %+v", l)
	}
	l, err := rt.listCommentBans(ctx, ScopeGlobal, 10, 0)
	if err != nil || len(l) != 1 || l[0].UserID != fan.ID || l[0].BannedBy != "" {
		t.Fatalf("global scope after erasing troll and the operator: %+v %v", l, err)
	}
	if err := rt.EraseSubjects(ctx, []string{owner.ID}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.commentBans+` WHERE scope = $1`, OwnerScope(owner.ID)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an erased owner's scope keeps %d bans (%v)", n, err)
	}
	if rec := doJSON(t, h, other, "PUT", "/comment-bans/"+troll.ID, banInput{}); rec.Code != http.StatusForbidden {
		t.Fatalf("banning an erased user: %d %s", rec.Code, rec.Body.String())
	}
}
