package content

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
)

// What signed-out visitors may do is the server's setting: off by default,
// each interaction on its own, refused with 401 unauthorized, and said by
// GET /config and the comment standing.
func TestAnonymousParticipationIsAServerSetting(t *testing.T) {
	user := access.Actor{ID: "u1", Kind: "user"}
	anon := access.Actor{Anonymous: true, IP: "10.9.0.1"}
	setup := func(t *testing.T, a Anonymous) (http.Handler, Post, Poll, Comment) {
		res := &fakeResolver{}
		res.set("gallery", cid(1), true, true)
		rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery", "post"}, Anonymous: a,
			Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
		h := rt.Handler()
		staff := access.Actor{ID: "staff", Kind: "user"}
		post := decodePost(t, doJSON(t, h, staff, "POST", "/posts", PostInput{Title: ptr("Hello"), Body: ptr("world")}))
		res.set("post", post.ID, true, true)
		var poll Poll
		rec := doJSON(t, h, staff, "POST", "/polls", PollInput{Question: "Which?", Language: "en",
			Options: []PollOptionInput{{Label: "A"}, {Label: "B", Position: 1}}})
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &poll) != nil {
			t.Fatalf("create poll: %d %s", rec.Code, rec.Body)
		}
		var cm Comment
		rec = doJSON(t, h, user, "POST", gallery(1, "/comments"), CommentInput{Body: "hi"})
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &cm) != nil {
			t.Fatalf("signed-in comment: %d %s", rec.Code, rec.Body)
		}
		return h, post, poll, cm
	}
	type attempt struct {
		name, method, path string
		body               any
		allowed            func(Anonymous) bool
	}
	attempts := func(post Post, poll Poll, cm Comment) []attempt {
		comments := func(a Anonymous) bool { return a.Comments }
		reactions := func(a Anonymous) bool { return a.Reactions }
		return []attempt{
			{"comment", "POST", gallery(1, "/comments"), CommentInput{Body: "drive-by", AnonName: "Guest"}, comments},
			{"reply", "POST", gallery(1, "/comments"), CommentInput{Body: "re", AnonName: "Guest", ReplyToID: cm.ID}, comments},
			{"work like", "POST", gallery(1, "/like"), nil, reactions},
			{"work reaction clear", "DELETE", gallery(1, "/reaction"), nil, reactions},
			{"post like by kind", "POST", "/post/" + post.ID + "/dislike", nil, reactions},
			{"comment like", "POST", "/comments/" + cm.ID + "/like", nil, reactions},
			{"vote", "POST", "/polls/" + poll.ID + "/vote", PollVote{OptionID: poll.Options[0].ID}, func(a Anonymous) bool { return a.Votes }},
		}
	}
	for _, a := range []Anonymous{{}, {Comments: true}, {Reactions: true}, {Votes: true}, {Comments: true, Reactions: true, Votes: true}} {
		h, post, poll, cm := setup(t, a)
		var cfg Config
		if rec := doJSON(t, h, anon, "GET", "/config", nil); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &cfg) != nil || cfg.Anonymous != a {
			t.Fatalf("%+v: GET /config = %d %s", a, rec.Code, rec.Body)
		}
		for _, at := range attempts(post, poll, cm) {
			rec := doJSON(t, h, anon, at.method, at.path, at.body)
			switch {
			case at.allowed(a) && (rec.Code < 200 || rec.Code > 299):
				t.Errorf("%+v %s: %d %s, want success", a, at.name, rec.Code, rec.Body)
			case !at.allowed(a) && (rec.Code != http.StatusUnauthorized || errCode(t, rec.Body.Bytes()) != CodeUnauthorized):
				t.Errorf("%+v %s: %d %s, want 401 unauthorized", a, at.name, rec.Code, rec.Body)
			}
		}
		// Signed in, every interaction stays open.
		if rec := doJSON(t, h, user, "POST", gallery(1, "/like"), nil); rec.Code != http.StatusOK {
			t.Fatalf("%+v: signed-in like: %d %s", a, rec.Code, rec.Body)
		}
		var standing CommentStanding
		if rec := doJSON(t, h, anon, "GET", gallery(1, "/can-comment"), nil); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &standing) != nil {
			t.Fatalf("%+v: standing: %d %s", a, rec.Code, rec.Body)
		}
		if standing.Anonymous != a.Comments || standing.CanComment != a.Comments {
			t.Fatalf("%+v: anonymous standing %+v", a, standing)
		}
		if rec := doJSON(t, h, user, "GET", gallery(1, "/can-comment"), nil); json.Unmarshal(rec.Body.Bytes(), &standing) != nil || !standing.CanComment || standing.Anonymous != a.Comments {
			t.Fatalf("%+v: signed-in standing %s", a, rec.Body)
		}
	}
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %s: %v", body, err)
	}
	return e.Code
}

// The longest comment is the server's: counted in characters as written,
// refused with comment_too_long and its max, and said by /config and the standing.
func TestCommentLengthIsAServerSetting(t *testing.T) {
	user := access.Actor{ID: "u1", Kind: "user"}
	for _, c := range []struct{ option, max int }{{0, DefaultCommentMaxLength}, {10, 10}} {
		res := &fakeResolver{}
		res.set("gallery", cid(1), true, true)
		rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}, CommentMaxLength: c.option})
		h := rt.Handler()
		var cfg Config
		if rec := doJSON(t, h, user, "GET", "/config", nil); json.Unmarshal(rec.Body.Bytes(), &cfg) != nil || cfg.CommentMaxLength != c.max {
			t.Fatalf("config %s", rec.Body)
		}
		var standing CommentStanding
		if rec := doJSON(t, h, user, "GET", gallery(1, "/can-comment"), nil); json.Unmarshal(rec.Body.Bytes(), &standing) != nil || standing.MaxLength != c.max {
			t.Fatalf("standing %s", rec.Body)
		}
		fits := "  " + strings.Repeat("é", c.max) + "\n"
		rec := doJSON(t, h, user, "POST", gallery(1, "/comments"), CommentInput{Body: fits})
		var cm Comment
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &cm) != nil {
			t.Fatalf("max %d: a comment of %d characters: %d %s", c.max, c.max, rec.Code, rec.Body)
		}
		long := strings.Repeat("é", c.max+1)
		for _, r := range []struct{ method, path string }{{"POST", gallery(1, "/comments")}, {"PATCH", "/comments/" + cm.ID}} {
			rec := doJSON(t, h, user, r.method, r.path, CommentInput{Body: long})
			var e struct {
				Code    string `json:"code"`
				Details struct {
					Max int `json:"max"`
				} `json:"details"`
			}
			if rec.Code != http.StatusUnprocessableEntity || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Code != CodeCommentTooLong || e.Details.Max != c.max {
				t.Fatalf("max %d: %s %s of %d characters: %d %s", c.max, r.method, r.path, c.max+1, rec.Code, rec.Body)
			}
		}
	}
	if _, err := New(context.Background(), Options{Pool: pgtest.Pool(t, nil), Schema: "x", Tenant: "t", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{}, CommentMaxLength: -1}); err == nil {
		t.Fatal("a negative CommentMaxLength was accepted")
	}
}
