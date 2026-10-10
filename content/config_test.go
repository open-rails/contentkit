package content

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-rails/contentkit/access"
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
			{"post like", "POST", "/posts/" + post.ID + "/like", nil, reactions},
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
