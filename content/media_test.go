package content

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

var mediaAdmin = access.Actor{ID: "admin", Kind: "user"}

func insertPost(t *testing.T, rt *Runtime) string {
	t.Helper()
	var id string
	if err := rt.store.pool.QueryRow(context.Background(),
		`INSERT INTO `+rt.store.t.posts+` (tenant_id, author_id, title, body) VALUES ($1,'admin','t','b') RETURNING id::text`, rt.tenant).Scan(&id); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	return id
}

func newMediaTest(t *testing.T, opts Options) (*Runtime, *testMedia) {
	t.Helper()
	m := &testMedia{}
	if opts.Media == nil {
		opts.Media = m.options()
	}
	opts.Perms = Perms{PostWrite: "post", PollWrite: "poll"}
	rt, _ := newTestRuntime(t, opts)
	return rt, m
}

// send serves one JSON request as actor and decodes a 2xx body into out.
func send(t *testing.T, rt *Runtime, actor access.Actor, method, path string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req = req.WithContext(withActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code < 300 && out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return rec.Code
}

func image(name string) map[string]string { return map[string]string{"image": name} }

func TestMedia_PostCoverAndInlineImages(t *testing.T) {
	rt, _ := newMediaTest(t, Options{})
	id := insertPost(t, rt)
	name := "i-" + uuid.NewString()
	want := "https://media.test/" + testTenant + "/post/" + id + "/public/" + name + ".webp"

	var cover map[string]*string
	if code := send(t, rt, mediaAdmin, "PUT", "/posts/"+id+"/cover", image(name), &cover); code != 200 || *cover["cover_url"] != want {
		t.Fatalf("cover %d %v", code, cover)
	}
	var v postView
	send(t, rt, mediaAdmin, "GET", "/posts/"+id, nil, &v)
	if v.CoverURL == nil || *v.CoverURL != want {
		t.Fatalf("stored cover %v", v.CoverURL)
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/posts/"+id+"/cover", image(""), nil); code != 200 {
		t.Fatalf("clear %d", code)
	}
	send(t, rt, mediaAdmin, "GET", "/posts/"+id, nil, &v)
	if v.CoverURL != nil {
		t.Fatalf("cover not cleared: %v", *v.CoverURL)
	}

	var inline map[string]string
	if code := send(t, rt, mediaAdmin, "POST", "/posts/"+id+"/images", image(name), &inline); code != 200 || inline["url"] != want {
		t.Fatalf("inline %d %v", code, inline)
	}
	for _, tc := range []struct {
		path string
		body any
		want int
	}{
		{"/posts/" + id + "/images", image("https://evil.example/x.png"), 400},
		{"/posts/" + id + "/images", image("cover"), 400},
		{"/posts/" + id + "/images", image(""), 400},
		{"/posts/" + id + "/images", map[string]string{}, 400},
		{"/posts/" + uuid.NewString() + "/images", image(name), 404},
	} {
		if code := send(t, rt, mediaAdmin, "POST", tc.path, tc.body, nil); code != tc.want {
			t.Errorf("POST %s %v: %d, want %d", tc.path, tc.body, code, tc.want)
		}
	}
	// A cover is not a post field: it is set only from the post's own folder.
	for _, m := range []string{"POST /posts", "PATCH /posts/" + id} {
		method, path, _ := strings.Cut(m, " ")
		if code := send(t, rt, mediaAdmin, method, path, map[string]any{"title": "t", "body": "b", "cover_url": "https://evil.example/x.png"}, nil); code != 400 {
			t.Errorf("%s with cover_url: %d", m, code)
		}
	}
}

func TestMedia_PollImages(t *testing.T) {
	rt, _ := newMediaTest(t, Options{})
	poll, err := rt.polls.create(context.Background(), mediaAdmin, createPollInput{Question: "Q?", Options: []createOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := rt.polls.create(context.Background(), mediaAdmin, createPollInput{Question: "R?", Options: []createOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	q, o := "i-"+uuid.NewString(), "i-"+uuid.NewString()
	folder := "https://media.test/" + testTenant + "/poll/" + poll.ID + "/public/"
	oid := poll.Options[0].ID
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/image", image(q), nil); code != 200 {
		t.Fatalf("question image %d", code)
	}
	var got map[string]*string
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/options/"+oid+"/image", image(o), &got); code != 200 || *got["image_url"] != folder+o+".webp" {
		t.Fatalf("option image %d %v", code, got)
	}
	v, _ := rt.polls.get(context.Background(), mediaAdmin, poll.ID)
	if v.ImageURL != folder+q+".webp" || v.Options[0].ImageURL != folder+o+".webp" && v.Options[1].ImageURL != folder+o+".webp" {
		t.Fatalf("stored %+v", v)
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+other.ID+"/options/"+oid+"/image", image(o), nil); code != 404 {
		t.Fatalf("option of another poll: %d", code)
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/image", image("../x"), nil); code != 400 {
		t.Fatalf("bad name: %d", code)
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/options/"+oid+"/image", image(""), nil); code != 200 {
		t.Fatalf("clear option: %d", code)
	}
	v, _ = rt.polls.get(context.Background(), mediaAdmin, poll.ID)
	for _, opt := range v.Options {
		if opt.ImageURL != "" {
			t.Fatalf("option image not cleared: %+v", opt)
		}
	}
}

func TestMedia_DeleteRemovesFolders(t *testing.T) {
	rt, m := newMediaTest(t, Options{})
	post := insertPost(t, rt)
	poll, err := rt.polls.create(context.Background(), mediaAdmin, createPollInput{Question: "Q?", Options: []createOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	if code := send(t, rt, mediaAdmin, "DELETE", "/posts/"+post, nil, nil); code != 200 {
		t.Fatalf("delete post %d", code)
	}
	if code := send(t, rt, mediaAdmin, "DELETE", "/polls/"+poll.ID, nil, nil); code >= 300 {
		t.Fatalf("delete poll %d", code)
	}
	want := []string{testTenant + "/post/" + post, testTenant + "/poll/" + poll.ID}
	if got := m.deletions(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
}

func TestMedia_CanUpload(t *testing.T) {
	ctx := context.Background()
	rt, _ := newMediaTest(t, Options{Media: (&testMedia{}).options()})
	post := insertPost(t, rt)
	poll, err := rt.polls.create(ctx, mediaAdmin, createPollInput{Question: "Q?", Options: []createOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref  contentref.ContentRef
		want bool
	}{
		{rt.Ref("post", post), true},
		{rt.Ref("poll", poll.ID), true},
		{rt.Ref("post", uuid.NewString()), false},
		{rt.Ref("poll", uuid.NewString()), false},
		{rt.Ref("gallery", post), false},
		{contentref.New("other", "post", post), false},
		{rt.Ref("post", post).WithVersion("v1"), false},
	} {
		g, err := rt.CanUpload(ctx, mediaAdmin, tc.ref)
		if err != nil || g.Allowed != tc.want {
			t.Errorf("%s: %+v %v, want %v", tc.ref, g, err, tc.want)
		}
	}
	denied, _ := newTestRuntime(t, Options{Authz: denyAll{}, Media: (&testMedia{}).options(), Perms: Perms{PostWrite: "post"}})
	if g, err := denied.CanUpload(ctx, mediaAdmin, denied.Ref("post", insertPost(t, denied))); err != nil || g.Allowed {
		t.Fatalf("without PostWrite: %+v %v", g, err)
	}
}

func TestMedia_ImageRoutesRequirePerm(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{Authz: denyAll{}, Media: (&testMedia{}).options(), Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
	id := insertPost(t, rt)
	if code := send(t, rt, access.Actor{ID: "nonadmin"}, "PUT", "/posts/"+id+"/cover", image("i-"+uuid.NewString()), nil); code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", code)
	}
}

// A foreign tenant's admin cannot point another tenant's post or poll at an image.
func TestMedia_ForeignTenantIsNotFound(t *testing.T) {
	ctx := context.Background()
	m := &testMedia{}
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Media: m.options(), Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
	b, err := New(ctx, Options{Pool: pool, Schema: a.schema, Tenant: "site_b", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{},
		Media: m.options(), Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
	if err != nil {
		t.Fatal(err)
	}
	post := insertPost(t, a)
	poll, err := a.polls.create(ctx, mediaAdmin, createPollInput{Question: "Q", Options: []createOptionInput{{Label: "a"}, {Label: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	name := image("i-" + uuid.NewString())
	for _, path := range []string{"/posts/" + post + "/cover", "/polls/" + poll.ID + "/image", "/polls/" + poll.ID + "/options/" + poll.Options[0].ID + "/image"} {
		if code := send(t, b, mediaAdmin, "PUT", path, name, nil); code != http.StatusNotFound {
			t.Errorf("PUT %s from another tenant: %d", path, code)
		}
	}
	if code := send(t, b, mediaAdmin, "DELETE", "/posts/"+post, nil, nil); code != http.StatusNotFound || len(m.deletions()) != 0 {
		t.Fatalf("foreign delete: %d %v", code, m.deletions())
	}
}
