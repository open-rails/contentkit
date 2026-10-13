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
	"github.com/open-rails/contentkit/internal/testauth"
)

// posted is a comment write's answer.
type posted struct {
	code    int
	comment Comment
	err     errorBody
}

func record(rec *httptest.ResponseRecorder) posted {
	p := posted{code: rec.Code}
	if rec.Code < 300 {
		_ = json.Unmarshal(rec.Body.Bytes(), &p.comment)
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &p.err)
	}
	return p
}

// Banned words and IP bans are staff-edited data the built-in moderation
// applies to every comment and free-text answer: masked words keep the rest
// of the text, reject words refuse it, a banned address or network cannot
// write until the ban ends or is lifted, and moderators are exempt.
func TestModerationRules(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("gallery"), Auth: testauth.Auth{Authz: rulesAuthz{}}})
	h := rt.Handler()
	staff, user := access.Actor{ID: "staff", Kind: "user", IP: "198.51.100.9"}, access.Actor{ID: "u1", Kind: "user", IP: "203.0.113.7"}
	post := func(who access.Actor, body string) posted {
		t.Helper()
		return record(doJSON(t, h, who, "POST", "/contents/"+cid(1)+"/comments", CommentInput{Body: body}))
	}

	if rec := doJSON(t, h, user, "POST", "/moderation/banned-words", []BannedWordInput{{Word: "x"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a user edits banned words: %d", rec.Code)
	}
	var words []BannedWord
	rec := doJSON(t, h, staff, "POST", "/moderation/banned-words", []BannedWordInput{{Word: "Darn"}, {Word: "Spamword", Action: WordReject}, {Word: "darn"}})
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &words) != nil || len(words) != 2 || words[0].Word != "darn" || words[0].Action != WordMask ||
		words[1].Action != WordReject || words[0].CreatedBy != "staff" {
		t.Fatalf("ban words: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, staff, "POST", "/moderation/banned-words", []BannedWordInput{{Word: "x", Action: "shout"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", rec.Code)
	}
	if c := post(user, "oh DARN it, darnit, ｄａｒｎ!"); c.code != http.StatusCreated || c.comment.Body != "oh **** it, darnit, *****" {
		t.Fatalf("masked comment: %d %+v", c.code, c.comment)
	}
	if c := post(user, "buy spamword now"); c.code != http.StatusUnprocessableEntity || c.err.Code != CodeModerationRejected {
		t.Fatalf("reject word: %d %+v", c.code, c.err)
	}
	if rec := doJSON(t, h, staff, "DELETE", "/moderation/banned-words", []string{"DARN"}); rec.Code != http.StatusNoContent {
		t.Fatalf("unban: %d %s", rec.Code, rec.Body)
	}
	if c := post(user, "darn"); c.comment.Body != "darn" {
		t.Fatalf("an unbanned word stays masked: %+v", c.comment)
	}

	until := time.Now().Add(time.Hour)
	var bans []IPBan
	rec = doJSON(t, h, staff, "POST", "/moderation/ip-bans", []IPBanInput{{Network: "203.0.113.0/24", Reason: "spam wave", Until: &until}, {Network: "198.51.100.9"}})
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &bans) != nil || len(bans) != 2 {
		t.Fatalf("ban IPs: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, staff, "POST", "/moderation/ip-bans", []IPBanInput{{Network: "not-an-ip"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad network: %d", rec.Code)
	}
	c := post(user, "hello")
	if c.code != http.StatusForbidden || c.err.Code != CodeCommentBanned || c.err.Ban == nil || c.err.Ban.Scope != ScopeIP || c.err.Ban.Reason != "spam wave" {
		t.Fatalf("IP-banned comment: %d %+v", c.code, c.err)
	}
	var standing CommentStanding
	if rec := doJSON(t, h, user, "GET", "/contents/"+cid(1)+"/can-comment", nil); json.Unmarshal(rec.Body.Bytes(), &standing) != nil ||
		standing.CanComment || standing.Ban == nil || standing.Ban.Scope != ScopeIP {
		t.Fatalf("standing under an IP ban: %s", rec.Body)
	}
	if c := post(staff, "moderators are exempt"); c.code != http.StatusCreated {
		t.Fatalf("a banned moderator's comment: %d %+v", c.code, c.err)
	}
	if c := post(access.Actor{ID: "u2", Kind: "user", IP: "203.0.114.1"}, "outside the network"); c.code != http.StatusCreated {
		t.Fatalf("an address outside the network: %d %+v", c.code, c.err)
	}
	if rec := doJSON(t, h, staff, "DELETE", "/moderation/ip-bans", []string{"203.0.113.0/24"}); rec.Code != http.StatusNoContent {
		t.Fatalf("lift: %d %s", rec.Code, rec.Body)
	}
	if c := post(user, "free again"); c.code != http.StatusCreated {
		t.Fatalf("after the ban was lifted: %d %+v", c.code, c.err)
	}
	if rec := doJSON(t, h, staff, "GET", "/moderation/ip-bans", nil); json.Unmarshal(rec.Body.Bytes(), &bans) != nil || len(bans) != 1 || bans[0].Network != "198.51.100.9/32" {
		t.Fatalf("remaining bans: %s", rec.Body)
	}
}

// rulesAuthz grants "staff" every permission.
type rulesAuthz struct{}

func (rulesAuthz) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff", nil
}

// Comments carry a language: a list narrows to one language's thread, a reply
// joins its comment's, and an unknown language is refused.
func TestCommentLanguages(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("gallery")})
	h := rt.Handler()
	u := access.Actor{ID: "u1", Kind: "user"}
	path := "/contents/" + cid(1) + "/comments"
	var en, ja Comment
	for _, c := range []struct {
		in  CommentInput
		out *Comment
	}{{CommentInput{Body: "hello", Language: "en"}, &en}, {CommentInput{Body: "konnichiwa", Language: "ja"}, &ja}, {CommentInput{Body: "no language"}, &Comment{}}} {
		rec := doJSON(t, h, u, "POST", path, c.in)
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), c.out) != nil || c.out.Language != c.in.Language {
			t.Fatalf("comment %+v: %d %s", c.in, rec.Code, rec.Body)
		}
	}
	if rec := doJSON(t, h, u, "POST", path, CommentInput{Body: "x", Language: "fr"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown language: %d", rec.Code)
	}
	if rec := doJSON(t, h, u, "POST", path, CommentInput{Body: "x", Language: "en", ReplyToID: ja.ID}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a reply in another language: %d", rec.Code)
	}
	var reply Comment
	if rec := doJSON(t, h, u, "POST", path, CommentInput{Body: "re", ReplyToID: ja.ID}); rec.Code != http.StatusCreated ||
		json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.Language != "ja" {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	list := func(query string) []string {
		t.Helper()
		var out []Comment
		rec := doJSON(t, h, u, "GET", path+query, nil)
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list %s: %d %s", query, rec.Code, rec.Body)
		}
		var bodies []string
		for _, c := range out {
			bodies = append(bodies, c.Body)
		}
		return bodies
	}
	if got := strings.Join(list("?language=ja"), ","); got != "konnichiwa" {
		t.Fatalf("ja thread = %s", got)
	}
	if got := strings.Join(list(""), ","); got != "no language,konnichiwa,hello" {
		t.Fatalf("every language = %s", got)
	}
	var feed []FeedItem
	if rec := doJSON(t, h, u, "GET", "/comments/latest?language=en", nil); json.Unmarshal(rec.Body.Bytes(), &feed) != nil || len(feed) != 1 || feed[0].Body != "hello" {
		t.Fatalf("latest en: %s", rec.Body)
	}
	if c := countsOf(t, rt, rt.Ref("gallery", cid(1))); c.CommentCount != 3 {
		t.Fatalf("comment count across languages = %d", c.CommentCount)
	}
}
