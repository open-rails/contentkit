package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/testauth"
)

// The routes: creators and staff write through them, everyone reads what is
// live, and refusals answer the declared codes.
func TestRoutesIntegration(t *testing.T) {
	f := newFixture(t, nil)
	var c Content
	f.want(http.StatusUnauthorized, anon, "POST", "/v1/contents", ContentInput{Kind: "gallery"}, nil)
	f.want(http.StatusForbidden, "bob", "POST", "/v1/contents", gallery("x", "en"), nil)
	f.want(http.StatusBadRequest, "staff", "POST", "/v1/contents", ContentInput{Kind: "nope"}, nil)
	f.want(http.StatusBadRequest, "staff", "POST", "/v1/contents", map[string]any{"kind": "gallery", "unknown": 1}, nil)
	in := gallery("Hello World", "en")
	in.Creators = &[]Creator{{UserID: "alice", Role: "artist"}}
	f.want(http.StatusCreated, "staff", "POST", "/v1/contents", in, &c)
	if c.State != StateDraft || !c.Editor || c.Code == "" || c.Path != "/en/g/"+c.Code+"/hello-world" || c.Creators[0].Role != "artist" || c.Revision < 1 {
		t.Fatalf("created %+v", c)
	}
	id := c.ID
	f.want(http.StatusNotFound, anon, "GET", "/v1/contents/"+id, nil, nil)
	f.want(http.StatusOK, "alice", "GET", "/v1/contents/"+id, nil, nil)

	// Text per language, with the viewer's language and fallback.
	f.want(http.StatusOK, "alice", "PUT", "/v1/contents/"+id+"/text/ja", Text{Title: "こんにちは", Description: "説明", Slug: "Konnichiwa"}, &c)
	if c.Text["ja"].Slug != "konnichiwa" || c.Text["en"].Title != "Hello World" {
		t.Fatalf("text %+v", c.Text)
	}
	f.want(http.StatusBadRequest, "alice", "PUT", "/v1/contents/"+id+"/text/de", Text{Title: "Hallo"}, nil)
	f.want(http.StatusForbidden, "bob", "PUT", "/v1/contents/"+id+"/text/en", Text{Title: "Mine"}, nil)

	// Versions: add, edit, segments, order, delete.
	var v Version
	f.want(http.StatusCreated, "staff", "POST", "/v1/contents/"+id+"/versions", VersionInput{Languages: []string{"ja"}, Label: ptr("JA"),
		Text: map[string]VersionText{"ja": {Title: "日本語版"}}}, &v)
	// An added version is a draft until it is published.
	if v.Position != 1 || v.Label != "JA" || v.Text["ja"].Title != "日本語版" || v.State != StateDraft {
		t.Fatalf("version %+v", v)
	}
	f.want(http.StatusOK, "staff", "PATCH", "/v1/contents/"+id+"/versions/"+v.ID, VersionInput{Traits: &[]string{"colored"}, State: StatePublished}, &v)
	f.want(http.StatusOK, "staff", "PUT", "/v1/contents/"+id+"/versions/"+v.ID+"/segments", SegmentsInput{Segments: []Segment{{Start: 0, End: 4}, {Start: 4, End: 9, Title: "Two"}}}, &v)
	if len(v.Segments) != 2 || v.Segments[1].Title != "Two" || !v.Segments[1].Released {
		t.Fatalf("segments %+v", v.Segments)
	}
	f.want(http.StatusBadRequest, "staff", "PUT", "/v1/contents/"+id+"/versions/order", OrderInput{IDs: []string{v.ID}}, nil)
	first := c.Versions[0].ID
	f.want(http.StatusOK, "staff", "PUT", "/v1/contents/"+id+"/versions/order", OrderInput{IDs: []string{v.ID, first}}, &c)
	if c.Versions[0].ID != v.ID {
		t.Fatalf("order %+v", c.Versions)
	}
	f.want(http.StatusNotFound, "staff", "PATCH", "/v1/contents/"+id+"/versions/"+id, VersionInput{Label: ptr("x")}, nil)

	// Publish: live for everyone, text in the viewer's language.
	f.want(http.StatusOK, "staff", "POST", "/v1/contents/"+id+"/publish", nil, &c)
	if !c.Live || c.State != StatePublished {
		t.Fatalf("published %+v", c)
	}
	var ja Content
	f.want(http.StatusOK, anon, "GET", "/v1/contents/"+id+"?language=ja", nil, &ja)
	if ja.Title != "こんにちは" || ja.Language != "ja" || ja.Path != "/ja/g/"+ja.Code+"/konnichiwa" || ja.Editor || ja.Text != nil || ja.Versions[0].Title != "日本語版" || ja.Versions[0].State != "" || ja.Versions[0].Text != nil {
		t.Fatalf("anonymous read in Japanese %+v", ja)
	}
	var accept Content
	r := f.doWith(anon, "GET", "/v1/contents/"+id, map[string]string{"Accept-Language": "ja-JP,en;q=0.5"})
	if json.Unmarshal(r.Body.Bytes(), &accept) != nil || accept.Language != "ja" {
		t.Fatalf("Accept-Language %s", r.Body)
	}
	var one Version
	f.want(http.StatusOK, anon, "GET", "/v1/contents/"+id+"/versions/"+first, nil, &one)
	if one.ID != first || one.Decision.Allowed != true {
		t.Fatalf("one version %+v", one)
	}
	f.want(http.StatusNotFound, anon, "GET", "/v1/contents/"+id+"/versions/"+id, nil, nil)
	f.want(http.StatusBadRequest, anon, "GET", "/v1/contents/"+id+"?language=xx", nil, nil)
	// The site's default language is prefixed however it is asked for.
	var en Content
	f.want(http.StatusOK, anon, "GET", "/v1/contents/"+id+"?language=en", nil, &en)
	r = f.doWith(anon, "GET", "/v1/contents/"+id, map[string]string{"Accept-Language": "en"})
	if json.Unmarshal(r.Body.Bytes(), &accept) != nil || en.Path != "/en/g/"+en.Code+"/hello-world" || accept.Path != en.Path {
		t.Fatalf("default language paths %q, Accept-Language %q", en.Path, accept.Path)
	}

	// Edits: listing, creators, a stale revision.
	f.want(http.StatusOK, "alice", "PATCH", "/v1/contents/"+id, ContentPatch{Listing: ptr(ListingUnlisted)}, &c)
	f.want(http.StatusConflict, "alice", "PATCH", "/v1/contents/"+id, ContentPatch{Listing: ptr(ListingListed), Revision: ptr(c.Revision - 1)}, nil)
	f.want(http.StatusOK, "alice", "PATCH", "/v1/contents/"+id, ContentPatch{Listing: ptr(ListingListed), Revision: ptr(c.Revision)}, nil)
	f.want(http.StatusOK, "staff", "PUT", "/v1/contents/"+id+"/creators", CreatorsInput{Creators: []Creator{{UserID: "carol"}}}, &c)
	f.want(http.StatusForbidden, "alice", "PATCH", "/v1/contents/"+id, ContentPatch{Listing: ptr(ListingUnlisted)}, nil)
	f.want(http.StatusNotImplemented, "staff", "POST", "/v1/contents/"+id+"/images", BodyImageInput{Image: ptr("i-01920000-0000-7000-8000-000000000001")}, nil)
	f.want(http.StatusBadRequest, "staff", "POST", "/v1/contents/"+id+"/images", BodyImageInput{Image: ptr("../x")}, nil)

	// Schedule, unpublish, delete, restore.
	at := f.now().Add(time.Hour).UTC().Truncate(time.Second)
	f.want(http.StatusOK, "staff", "POST", "/v1/contents/"+id+"/unpublish", nil, nil)
	f.want(http.StatusConflict, "staff", "POST", "/v1/contents/"+id+"/unpublish", nil, nil)
	f.want(http.StatusOK, "staff", "POST", "/v1/contents/"+id+"/publish", PublishInput{PublishAt: &at}, &c)
	if c.Live || c.PublishAt == nil || !c.PublishAt.Equal(at) {
		t.Fatalf("scheduled %+v", c)
	}
	f.want(http.StatusNotFound, anon, "GET", "/v1/contents/"+id, nil, nil)
	f.want(http.StatusNoContent, "staff", "DELETE", "/v1/contents/"+id+"/versions/"+v.ID, nil, nil)
	f.want(http.StatusConflict, "staff", "DELETE", "/v1/contents/"+id+"/versions/"+first, nil, nil)
	f.want(http.StatusOK, "staff", "DELETE", "/v1/contents/"+id+"/text/ja", nil, nil)
	f.want(http.StatusNoContent, "staff", "DELETE", "/v1/contents/"+id, nil, nil)
	f.want(http.StatusNotFound, "staff", "DELETE", "/v1/contents/"+id, nil, nil)
	f.want(http.StatusOK, "staff", "POST", "/v1/contents/"+id+"/restore", nil, &c)
	if c.State != StatePublished || c.DeletedAt != nil {
		t.Fatalf("restored %+v", c)
	}

	// The manage list: staff see all; others what they created, or an owner
	// group's they edit.
	studio := f.group("channel", "studio")
	f.editor("studio", "ed")
	owned := gallery("Studio draft", "en")
	owned.OwnerGroupID = studio
	f.want(http.StatusCreated, "ed", "POST", "/v1/contents", owned, &c)
	f.want(http.StatusForbidden, "bob", "POST", "/v1/contents", owned, nil)
	var list []Content
	f.want(http.StatusOK, "staff", "GET", "/v1/manage/contents?kind=gallery", nil, &list)
	if len(list) != 2 || list[0].ID != c.ID {
		t.Fatalf("staff manage %+v", list)
	}
	f.want(http.StatusOK, "ed", "GET", "/v1/manage/contents?owner_group="+studio+"&state=draft", nil, &list)
	if len(list) != 1 || list[0].ID != c.ID || !list[0].Editor {
		t.Fatalf("editor manage %+v", list)
	}
	f.want(http.StatusOK, "carol", "GET", "/v1/manage/contents?state=scheduled", nil, &list)
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("creator manage %+v", list)
	}
	f.want(http.StatusOK, "staff", "GET", "/v1/manage/contents?q=WORLD", nil, &list)
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("text search %+v", list)
	}
	f.want(http.StatusForbidden, "bob", "GET", "/v1/manage/contents?owner_group="+studio, nil, nil)
	f.want(http.StatusBadRequest, "staff", "GET", "/v1/manage/contents?state=gone", nil, nil)
	f.want(http.StatusUnauthorized, anon, "GET", "/v1/manage/contents", nil, nil)
}

func (f *fixture) doWith(actor, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if actor != "" {
		req = testauth.As(req, access.Actor{ID: actor, Kind: "user"})
	}
	req.Host = "main.test"
	rec := httptest.NewRecorder()
	f.handler().ServeHTTP(rec, req)
	return rec
}

// moderator holds text containing "iffy", rejects "spam", and pauses on
// "paused" until released.
type moderator struct {
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (*moderator) StatelessPolicy() {}

func (m *moderator) Screen(_ context.Context, in content.ModerationInput) (content.Verdict, error) {
	switch {
	case strings.Contains(in.Text, "spam"):
		return content.Verdict{Decision: content.DecisionReject, Reason: "no spam"}, nil
	case strings.Contains(in.Text, "iffy"):
		return content.Verdict{Decision: content.DecisionReview, Reason: "needs a look", Model: "fake"}, nil
	case strings.Contains(in.Text, "paused"):
		close(m.entered)
		<-m.release
	}
	return content.Verdict{Decision: content.DecisionApprove}, nil
}

// Text is screened by content's moderator through the shared hold flow: a
// rejection writes nothing, a hold keeps a live item's approved text public
// while the edit waits in the review queue, and the reviewer's decision
// publishes it. A screen in flight never overwrites a newer edit.
func TestModerationHoldIntegration(t *testing.T) {
	mod := &moderator{entered: make(chan struct{}), release: make(chan struct{})}
	var rt *content.Runtime
	late := &lateResolver{}
	f := newFixture(t, func(o *Options) {
		var err error
		rt, err = content.New(t.Context(), content.Options{Pool: o.Pool, Schema: o.Schema, Tenant: tenant, Auth: o.Auth,
			Resolver: late, Kinds: o.Kinds, Moderator: mod, Limits: content.Limits{Disabled: true}})
		if err != nil {
			t.Fatal(err)
		}
		o.Moderation = rt
	})
	late.s = f.s
	if err := rt.AddHeldSource(HeldKind, f.s); err != nil {
		t.Fatal(err)
	}
	in := ContentInput{Kind: "blog", Text: map[string]Text{"en": {Title: "Post", Body: "first draft is iffy"}}}
	id := f.create("staff", in)
	// A draft is never screened; publishing screens every language.
	f.want(http.StatusAccepted, "staff", "POST", "/v1/contents/"+id+"/publish", nil, nil)
	if got := f.read(anon, "main.test", id)[0]; got != nil {
		t.Fatal("a held, never approved item is public")
	}
	page, err := rt.ListHeld(f.ctx, HeldKind, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].Reason != "needs a look" || page.Items[0].Model != "fake" || page.Items[0].Title != "Post" {
		t.Fatalf("held queue %+v %v", page, err)
	}
	if err := rt.Resolve(f.ctx, HeldKind, id, content.ReviewDecision{Revision: page.Items[0].Revision, Decision: content.DecisionApprove, Reviewer: "staff"}); err != nil {
		t.Fatal(err)
	}
	live := f.read(anon, "main.test", id)[0]
	if live == nil || !live.Live || live.Body != "first draft is iffy" {
		t.Fatalf("approved %+v", live)
	}

	// A rejected edit writes nothing; a held edit keeps the approved text public.
	f.want(http.StatusUnprocessableEntity, "staff", "PUT", "/v1/contents/"+id+"/text/en", Text{Title: "Post", Body: "buy spam"}, nil)
	f.want(http.StatusAccepted, "staff", "PUT", "/v1/contents/"+id+"/text/en", Text{Title: "Post v2", Body: "iffy again"}, nil)
	if got := f.read(anon, "main.test", id)[0]; got == nil || got.Title != "Post" || got.Body != "first draft is iffy" {
		t.Fatalf("the public sees %+v", got)
	}
	if got := f.read("staff", "main.test", id)[0]; got.Title != "Post v2" || got.Moderation != content.ModerationHeld || got.ModerationReason != "needs a look" {
		t.Fatalf("the editor sees %+v", got)
	}
	page, _ = rt.ListHeld(f.ctx, HeldKind, "", 10)
	if err := rt.Resolve(f.ctx, HeldKind, id, content.ReviewDecision{Revision: page.Items[0].Revision, Decision: content.DecisionReject, Reviewer: "staff", Reason: "no"}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(anon, "main.test", id)[0]; got == nil || got.Title != "Post" {
		t.Fatalf("after a rejected edit the public sees %+v", got)
	}
	if got := f.read("staff", "main.test", id)[0]; got.Moderation != content.ModerationRejected || got.ModerationReason != "no" {
		t.Fatalf("the editor sees the rejection %+v", got)
	}
	if err := rt.Resolve(f.ctx, HeldKind, id, content.ReviewDecision{Revision: page.Items[0].Revision, Decision: content.DecisionApprove, Reviewer: "staff"}); err == nil {
		t.Fatal("a decided item was resolved again")
	}

	// A paused screen loses to a newer edit.
	done := make(chan int, 1)
	go func() {
		done <- f.do("staff", "PUT", "/v1/contents/"+id+"/text/en", Text{Title: "Post", Body: "paused edit"}).Code
	}()
	<-mod.entered
	f.want(http.StatusOK, "staff", "PUT", "/v1/contents/"+id+"/text/en", Text{Title: "Post v3", Body: "clean"}, nil)
	close(mod.release)
	if code := <-done; code != http.StatusConflict {
		t.Fatalf("the paused edit answered %d, want 409", code)
	}
	if got := f.read(anon, "main.test", id)[0]; got.Title != "Post v3" || got.Body != "clean" {
		t.Fatalf("after the race %+v", got)
	}
}

// lateResolver resolves content refs through the catalog, built after the
// content runtime it serves.
type lateResolver struct{ s *Store }

func (l *lateResolver) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	return l.s.Resolver().Resolve(ctx, refs, a)
}
