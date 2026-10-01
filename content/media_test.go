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
	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/s3"
)

var mediaAdmin = access.Actor{ID: "admin", Kind: "user"}

func insertPost(t *testing.T, rt *Runtime) string {
	t.Helper()
	id := contentref.NewID()
	if _, err := rt.store.pool.Exec(context.Background(),
		`INSERT INTO `+rt.store.t.posts+` (id, tenant_id, author_id, title, body) VALUES ($2,$1,'admin','t','b')`, rt.tenant, id); err != nil {
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
	rt, m := newMediaTest(t, Options{})
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
	var stored string
	if err := rt.store.pool.QueryRow(t.Context(), `SELECT cover_name FROM `+rt.store.t.posts+` WHERE id=$1`, id).Scan(&stored); err != nil || stored != name {
		t.Fatalf("stored cover name %q, err=%v", stored, err)
	}
	m.origin = "https://moved-media.test"
	moved := strings.Replace(want, "https://media.test", m.origin, 1)
	if code := send(t, rt, mediaAdmin, "GET", "/posts/"+id, nil, &v); code != 200 || v.CoverURL == nil || *v.CoverURL != moved {
		t.Fatalf("cover after origin change: status=%d, cover=%v", code, v.CoverURL)
	}
	if code := send(t, rt, mediaAdmin, "PATCH", "/posts/"+id, map[string]bool{"is_draft": false}, &v); code != 200 || v.CoverURL == nil || *v.CoverURL != moved {
		t.Fatalf("published cover after origin change: status=%d, cover=%v", code, v.CoverURL)
	}
	var listed []postView
	if code := send(t, rt, mediaAdmin, "GET", "/posts", nil, &listed); code != 200 || len(listed) != 1 || listed[0].CoverURL == nil || *listed[0].CoverURL != moved {
		t.Fatalf("listed cover after origin change: status=%d, posts=%+v", code, listed)
	}
	if code := send(t, rt, mediaAdmin, "PUT", "/posts/"+id+"/cover", image(""), nil); code != 200 {
		t.Fatalf("clear %d", code)
	}
	var cleared postView
	send(t, rt, mediaAdmin, "GET", "/posts/"+id, nil, &cleared)
	if cleared.ID != id || cleared.CoverURL != nil {
		t.Fatalf("cover not cleared: %+v", cleared)
	}
	var clearedName *string
	if err := rt.store.pool.QueryRow(t.Context(), `SELECT cover_name FROM `+rt.store.t.posts+` WHERE id=$1`, id).Scan(&clearedName); err != nil || clearedName != nil {
		t.Fatalf("cleared cover name %v, err=%v", clearedName, err)
	}

	var inline map[string]string
	if code := send(t, rt, mediaAdmin, "POST", "/posts/"+id+"/images", image(name), &inline); code != 200 || inline["url"] != moved {
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
		{"/posts/" + contentref.NewID() + "/images", image(name), 404},
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
	rt, m := newMediaTest(t, Options{})
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
	var questionName, optionName string
	if err := rt.store.pool.QueryRow(t.Context(), `SELECT q.image_name, o.image_name FROM `+rt.store.t.pollQuestions+` q JOIN `+rt.store.t.pollOptions+` o ON o.question_id=q.id WHERE o.id=$1`, oid).Scan(&questionName, &optionName); err != nil || questionName != q || optionName != o {
		t.Fatalf("stored image names %q/%q, err=%v", questionName, optionName, err)
	}
	m.origin = "https://moved-media.test"
	movedFolder := strings.Replace(folder, "https://media.test", m.origin, 1)
	if v, err := rt.polls.get(t.Context(), mediaAdmin, poll.ID); err != nil || v.ImageURL != movedFolder+q+".webp" {
		t.Fatalf("poll after origin change: %+v, err=%v", v, err)
	}
	if listed, err := rt.polls.list(t.Context(), mediaAdmin, listFilter{limit: 10}); err != nil || len(listed) != 2 || listed[1].ImageURL != movedFolder+q+".webp" {
		t.Fatalf("poll list after origin change: %+v, err=%v", listed, err)
	}
	var edited pollOption
	if code := send(t, rt, mediaAdmin, "PATCH", "/polls/"+strings.ToUpper(poll.ID)+"/options/"+oid, map[string]string{"label": "edited"}, &edited); code != 200 || edited.ImageURL != movedFolder+o+".webp" {
		t.Fatalf("edited option after origin change: status=%d, option=%+v", code, edited)
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

func TestMedia_SoftDeleteHidesPostAndDeletesPoll(t *testing.T) {
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
	want := []string{testTenant + "/poll/" + poll.ID}
	if got := m.deletions(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	if want := []string{testTenant + "/post/" + post}; !slices.Equal(m.exposed, want) {
		t.Fatalf("exposed %v, want %v", m.exposed, want)
	}
}

func TestMedia_PostExposureCommitsWithContent(t *testing.T) {
	ctx := context.Background()
	ports := (&testMedia{}).options()
	ports.PostKind = "article"
	rt, pool := newTestRuntime(t, Options{Media: ports, Moderator: &fakeModerator{},
		Perms: Perms{PostWrite: "post", ModerationReview: "review"}})
	reg, err := media.NewRegistry(media.Config{Namespace: testTenant, Kinds: []media.Kind{{Name: "article",
		Uploads: []media.Upload{{Path: "images/{name}", Types: []string{"image/png"}, MaxBytes: 1024, Named: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	// Only transactionally queued jobs are exercised; workers and S3 are not started.
	store, err := s3.New(s3.Config{Bucket: "unused", Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: reg, Locker: media.PGLocker(pool)})
	if err != nil {
		t.Fatal(err)
	}
	if err := riverhelpers.ApplyMigrations(ctx, pool, rt.schema); err != nil {
		t.Fatal(err)
	}
	if _, err := riverhelpers.New(ctx, pool, &river.Config{Schema: rt.schema}, jobs.RiverJobs()); err != nil {
		t.Fatal(err)
	}
	rt.media.Folders = jobs
	schema := pgx.Identifier{rt.schema}.Sanitize()
	queue := schema + ".river_job"
	snapshot := func() string {
		t.Helper()
		var state string
		err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
			(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM `+rt.store.t.posts+` p),
			(SELECT jsonb_agg(to_jsonb(d) ORDER BY content_id, language) FROM `+schema+`.content_search_dirty d),
			(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM `+queue+` j))::text`).Scan(&state)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	var post postView
	for i, step := range []struct {
		name, method, path string
		body               any
		status             int
	}{
		{"create", "POST", "/posts", postWriteReq{Title: ptr("Title"), Body: ptr("iffy"), Language: ptr("en")}, 202},
		{"update", "PATCH", "/posts/{id}", postWriteReq{Body: ptr("iffy edit"), Language: ptr("ja")}, 202},
		{"approve", "POST", "/moderation/post/{id}/resolve", map[string]any{"revision": 2, "decision": "approve"}, 200},
		{"delete", "DELETE", "/posts/{id}", nil, 200},
	} {
		t.Run(step.name, func(t *testing.T) {
			path := strings.ReplaceAll(step.path, "{id}", post.ID)
			if _, err := pool.Exec(ctx, `ALTER TABLE `+queue+` ADD CONSTRAINT reject_exposure CHECK (kind <> 'contentkit_media_expose') NOT VALID`); err != nil {
				t.Fatal(err)
			}
			before := snapshot()
			if code := send(t, rt, mediaAdmin, step.method, path, step.body, nil); code != 500 {
				t.Fatalf("queue failure: status %d, want 500", code)
			}
			if after := snapshot(); after != before {
				t.Fatal("queue failure committed post, keyword or media job changes")
			}
			if _, err := pool.Exec(ctx, `ALTER TABLE `+queue+` DROP CONSTRAINT reject_exposure`); err != nil {
				t.Fatal(err)
			}
			var result postView
			if code := send(t, rt, mediaAdmin, step.method, path, step.body, &result); code != step.status {
				t.Fatalf("status %d, want %d", code, step.status)
			}
			if i == 0 {
				post = result
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+queue+` WHERE kind='contentkit_media_expose' AND args->'ref'=$1::jsonb`, rt.Ref("article", post.ID)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != i+1 {
				t.Fatalf("committed exposure jobs %d, want %d", count, i+1)
			}
		})
		if t.Failed() {
			break
		}
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
		{rt.Ref("post", contentref.NewID()), false},
		{rt.Ref("poll", contentref.NewID()), false},
		{rt.Ref("gallery", post), false},
		{contentref.New("other", "post", post), false},
		{rt.Ref("post", post).WithVersion("v1"), false},
	} {
		g, err := rt.CanUpload(ctx, mediaAdmin, media.UploadTarget{Ref: tc.ref})
		if err != nil || g.Allowed != tc.want {
			t.Errorf("%s: %+v %v, want %v", tc.ref, g, err, tc.want)
		}
	}
	denied, _ := newTestRuntime(t, Options{Authz: denyAll{}, Media: (&testMedia{}).options(), Perms: Perms{PostWrite: "post"}})
	if g, err := denied.CanUpload(ctx, mediaAdmin, media.UploadTarget{Ref: denied.Ref("post", insertPost(t, denied))}); err != nil || g.Allowed {
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
