package content

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
)

// mediaAdmin holds every permission when it calls the module directly; over
// HTTP the runtime's Authorizer decides.
var mediaAdmin = testauth.Actor(access.Actor{ID: "admin", Kind: "user"}, testauth.All{})

func newMediaTest(t *testing.T, opts Options) (*Runtime, *testMedia) {
	t.Helper()
	m := &testMedia{}
	if opts.Media == nil {
		opts.Media = m.options()
	}
	rt, _ := newTestRuntime(t, opts)
	return rt, m
}

// send serves one JSON request as actor and decodes a 2xx body into out.
func send(t *testing.T, rt *Runtime, actor access.Actor, method, path string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req = testauth.As(req, actor)
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

func TestMedia_PollImages(t *testing.T) {
	rt, m := newMediaTest(t, Options{})
	poll, err := rt.polls.create(context.Background(), mediaAdmin, PollInput{Question: "Q?", Options: []PollOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := rt.polls.create(context.Background(), mediaAdmin, PollInput{Question: "R?", Options: []PollOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	q, o := "i-"+uuid.NewString(), "i-"+uuid.NewString()
	ref := rt.Ref("poll", poll.ID)
	oid := poll.Options[0].ID
	var got map[string]*string
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/image", image(q), &got); code != 200 || got["image_url"] != nil {
		t.Fatalf("question image without a public file %d %v", code, got)
	}
	m.publish(q)
	m.publish(o)
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/options/"+oid+"/image", image(o), &got); code != 200 || *got["image_url"] != m.url(ref, o) {
		t.Fatalf("option image %d %v", code, got)
	}
	v, _ := rt.polls.get(context.Background(), mediaAdmin, poll.ID)
	if v.ImageURL != m.url(ref, q) || v.Options[0].ImageURL != m.url(ref, o) {
		t.Fatalf("stored %+v", v)
	}
	var questionName, optionName string
	if err := rt.store.pool.QueryRow(t.Context(), `SELECT q.image_name, o.image_name FROM `+rt.store.t.pollQuestions+` q JOIN `+rt.store.t.pollOptions+` o ON o.question_id=q.id WHERE o.id=$1`, oid).Scan(&questionName, &optionName); err != nil || questionName != q || optionName != o {
		t.Fatalf("stored image names %q/%q, err=%v", questionName, optionName, err)
	}
	m.origin = "https://moved-media.test"
	m.publish(q)
	if v, err := rt.polls.get(t.Context(), mediaAdmin, poll.ID); err != nil || v.ImageURL != m.url(ref, q) {
		t.Fatalf("poll after a new generation and origin: %+v, err=%v", v, err)
	}
	before := m.lookupCount()
	if listed, err := rt.polls.list(t.Context(), mediaAdmin, listFilter{limit: 10}); err != nil || len(listed) != 2 || listed[1].ImageURL != m.url(ref, q) || m.lookupCount() != before+1 {
		t.Fatalf("poll list: %+v, err=%v, lookups %d", listed, err, m.lookupCount()-before)
	}
	var edited PollOption
	if code := send(t, rt, mediaAdmin, "PATCH", "/polls/"+strings.ToUpper(poll.ID)+"/options/"+oid, map[string]string{"label": "edited"}, &edited); code != 200 || edited.ImageURL != m.url(ref, o) {
		t.Fatalf("edited option: status=%d, option=%+v", code, edited)
	}
	m.unpublish(o)
	if v, _ := rt.polls.get(t.Context(), mediaAdmin, poll.ID); v.Options[0].ImageURL != "" {
		t.Fatalf("option without a public file: %+v", v.Options[0])
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+other.ID+"/options/"+oid+"/image", image(o), nil); code != 404 {
		t.Fatalf("option of another poll: %d", code)
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/image", image("../x"), nil); code != 400 {
		t.Fatalf("bad name: %d", code)
	}
	m.publish(o)
	if code := send(t, rt, mediaAdmin, "PUT", "/polls/"+poll.ID+"/options/"+oid+"/image", image(""), &got); code != 200 || got["image_url"] != nil {
		t.Fatalf("clear option: %d %v", code, got)
	}
	v, _ = rt.polls.get(context.Background(), mediaAdmin, poll.ID)
	for _, opt := range v.Options {
		if opt.ImageURL != "" {
			t.Fatalf("option image not cleared: %+v", opt)
		}
	}
}

func TestMedia_CanUpload(t *testing.T) {
	ctx := context.Background()
	rt, _ := newMediaTest(t, Options{Media: (&testMedia{}).options()})
	poll, err := rt.polls.create(ctx, mediaAdmin, PollInput{Question: "Q?", Options: []PollOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref  contentref.ContentRef
		want bool
	}{
		{rt.Ref("poll", poll.ID), true},
		{rt.Ref("poll", contentref.NewID()), false},
		{rt.Ref("gallery", poll.ID), false},
		{contentref.New("other", "poll", poll.ID), false},
		{rt.Ref("poll", poll.ID).WithVersion("v1"), false},
	} {
		g, err := rt.CanUpload(ctx, mediaAdmin, media.UploadTarget{Ref: tc.ref})
		if err != nil || g.Allowed != tc.want {
			t.Errorf("%s: %+v %v, want %v", tc.ref, g, err, tc.want)
		}
	}
	denied, err := New(ctx, Options{Kinds: testKinds("gallery"), Pool: rt.store.pool, Schema: rt.schema, Tenant: rt.tenant, Auth: testauth.Auth{Authz: denyAll{}},
		Resolver: &fakeResolver{}, Media: (&testMedia{}).options()})
	if err != nil {
		t.Fatal(err)
	}
	if g, err := denied.CanUpload(ctx, denied.as(mediaAdmin), media.UploadTarget{Ref: denied.Ref("poll", poll.ID)}); err != nil || g.Allowed {
		t.Fatalf("without PollsWrite: %+v %v", g, err)
	}
}

// A foreign tenant's admin cannot point another tenant's poll at an image.
func TestMedia_ForeignTenantIsNotFound(t *testing.T) {
	ctx := context.Background()
	m := &testMedia{}
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Media: m.options()})
	b, err := New(ctx, Options{Kinds: testKinds("gallery"), Pool: pool, Schema: a.schema, Tenant: "site_b", Auth: testauth.Auth{Authz: allowAll{}}, Resolver: &fakeResolver{},
		Media: m.options()})
	if err != nil {
		t.Fatal(err)
	}
	poll, err := a.polls.create(ctx, mediaAdmin, PollInput{Question: "Q", Options: []PollOptionInput{{Label: "a"}, {Label: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	name := image("i-" + uuid.NewString())
	for _, path := range []string{"/polls/" + poll.ID + "/image", "/polls/" + poll.ID + "/options/" + poll.Options[0].ID + "/image"} {
		if code := send(t, b, mediaAdmin, "PUT", path, name, nil); code != http.StatusNotFound {
			t.Errorf("PUT %s from another tenant: %d", path, code)
		}
	}
	if code := send(t, b, mediaAdmin, "DELETE", "/polls/"+poll.ID, nil, nil); code != http.StatusNotFound || len(m.deletions()) != 0 {
		t.Fatalf("foreign delete: %d %v", code, m.deletions())
	}
}

func TestMedia_DeletingAPollRetiresItsFolder(t *testing.T) {
	rt, m := newMediaTest(t, Options{})
	poll, err := rt.polls.create(context.Background(), mediaAdmin, PollInput{Question: "Q?", Options: []PollOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	if code := send(t, rt, mediaAdmin, "DELETE", "/polls/"+poll.ID, nil, nil); code >= 300 {
		t.Fatalf("delete poll %d", code)
	}
	want := []string{testTenant + "/poll/" + poll.ID}
	if got := m.deletions(); !slices.Equal(got, want) {
		t.Fatalf("retired %v, want %v", got, want)
	}
	var tombstone bool
	var options int
	if err := rt.store.pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL,(SELECT count(*) FROM `+rt.store.t.pollOptions+` WHERE question_id=$1 AND tenant_id=$2) FROM `+rt.store.t.pollQuestions+` WHERE id=$1 AND tenant_id=$2`, poll.ID, rt.tenant).Scan(&tombstone, &options); err != nil || !tombstone || options != 2 {
		t.Fatalf("poll history lost before retirement: tombstone=%v options=%d err=%v", tombstone, options, err)
	}
}

// A poll's folder shows while the poll exists, edited by PollsWrite holders.
func TestMediaResolverShowsPollFolders(t *testing.T) {
	ctx := context.Background()
	rt, _ := newMediaTest(t, Options{Auth: testauth.Auth{Authz: roleAuthz{writers: map[string]bool{"admin": true}}}})
	var poll Poll
	if code := send(t, rt, mediaAdmin, "POST", "/polls", PollInput{Question: "Q", Language: "en",
		Options: []PollOptionInput{{Label: "A"}, {Label: "B", Position: 1}}}, &poll); code != http.StatusCreated {
		t.Fatalf("create poll: %d", code)
	}
	refs := []contentref.ContentRef{rt.Ref("poll", poll.ID), rt.Ref("poll", contentref.NewID()), rt.Ref("gallery", poll.ID), contentref.New("other", "poll", poll.ID)}
	full := func(editor bool) resolve.Resolution {
		return resolve.Resolution{Visible: true, Accessible: true, Editor: editor}
	}
	for _, c := range []struct {
		actor access.Actor
		want  resolve.Resolution
	}{{access.Actor{Anonymous: true}, full(false)}, {access.Actor{ID: "reader"}, full(false)}, {mediaAdmin, full(true)}} {
		got, err := rt.MediaResolver().Resolve(ctx, refs, c.actor)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !reflect.DeepEqual(got[refs[0].Key()], c.want) {
			t.Errorf("%s: %v, want only the poll as %+v", c.actor.ID, got, c.want)
		}
	}
}

// roleAuthz grants every permission to the actors in writers only.
type roleAuthz struct{ writers map[string]bool }

func (a roleAuthz) Can(_ context.Context, actor access.Actor, _ string) (bool, error) {
	return a.writers[actor.ID], nil
}
