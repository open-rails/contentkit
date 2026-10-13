package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
)

// ckStaff holds every content permission for "staff" only.
type ckStaff struct{}

func (ckStaff) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff" && !a.Anonymous, nil
}

// others is versions.Others: the poll kind's folders, through the content
// module, as a host wires them.
type others struct{ content *content.Runtime }

func (o *others) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	return o.content.MediaResolver().Resolve(ctx, refs, a)
}

func (o *others) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	return o.content.CanUpload(ctx, a, t)
}

type keepHTML struct{}

func (keepHTML) Sanitize(_ context.Context, raw string) (string, error) { return raw, nil }

// A content body stores references to its images, so every image it shows
// resolves to the current file through publish, a forced regenerate and an
// unpublish and publish (each a new generation, the old one retired), with
// the real worker rendering. A draft's images are its editors' alone.
func TestContentImagesResolveAcrossGenerations(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := t.Context()
	pool, schema := env.Pool(), env.ContentSchema()
	folder := func(name string) media.Kind {
		return media.Kind{Name: name, Uploads: []media.Upload{{Path: "{name}", Types: pngs, MaxBytes: 10 << 20, Named: true, Max: 20}},
			Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 320}}}}
	}
	// The blog's named content images make its body kind (blog.body) and
	// hold its cover; its hidden version takes attachments.
	blog := folder("blog")
	blog.Uploads[0].Content = true
	blog.Uploads = append(blog.Uploads, media.Upload{Path: "files/{name}", Types: pngs, MaxBytes: 10 << 20})
	site := kinds.Config{Languages: []string{"en", "ja"}, Kinds: map[string]kinds.Kind{
		"blog":    {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
		"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}}}}
	gallery := media.Kind{Name: "gallery", KeepOriginals: true, Uploads: []media.Upload{{Path: "pages/{name}", Types: pngs, MaxBytes: 10 << 20}}}
	auth := testauth.Auth{Authz: ckStaff{}}
	cat, err := catalog.New(ctx, catalog.Options{Pool: pool, Schema: schema, Tenant: env.Tenant, Auth: auth, Kinds: site, Body: keepHTML{},
		DeletedRetention: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	polls := &others{}
	svc, err := versions.New(versions.Options{Tenant: env.Tenant, Schema: schema, Pool: pool, Kinds: site, Auth: auth,
		Resolver: cat.Resolver(), Facets: cat, Others: polls})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := media.ForContent(media.Config{Namespace: env.Tenant, BaseURL: "https://media.example",
		Kinds: []media.Kind{blog, gallery, folder("poll")}, Hooks: svc.Hooks()}, site)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	workers := workerSchema(t, pool)
	if err := workqueue.Migrate(ctx, pool, workers); err != nil {
		t.Fatal(err)
	}
	queue, err := workqueue.New(pool, reg, workers)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := media.NewPGJournal(pool, schema, queue)
	if err != nil {
		t.Fatal(err)
	}
	hostRiver := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, hostRiver); err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: reg, Locker: s3test.Locker(t, env.Store), Journal: journal, Pool: pool, Processes: queue})
	if err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, pool, &river.Config{Schema: hostRiver, FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond}, jobs.RiverJobs(), svc.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	uploads, err := media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: jobs.Manifests(), Commits: media.RateLimit{Disabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: jobs.Manifests(), Queue: queue,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: token.Key{ID: "k", Secret: bytes.Repeat([]byte("k"), 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Bind(versions.Media{Manifests: jobs.Manifests(), Uploads: uploads, Jobs: jobs}); err != nil {
		t.Fatal(err)
	}
	if err := cat.BindMedia(catalog.Media{Images: jobs.Manifests(), Reader: reader, Versions: svc}); err != nil {
		t.Fatal(err)
	}
	rt, err := content.New(ctx, content.Options{Pool: pool, Schema: schema, Tenant: env.Tenant, Auth: auth,
		Resolver: cat.Resolver(), Kinds: site, Limits: content.Limits{Disabled: true},
		Media: &content.Media{Images: jobs.Manifests(), Folders: jobs, Reader: reader}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.AddHeldSource(catalog.HeldKind, cat); err != nil {
		t.Fatal(err)
	}
	cat.BindModeration(rt)
	polls.content = rt
	mux := http.NewServeMux()
	mux.Handle("/", rt.Handler())
	mux.Handle("/v1/", http.StripPrefix("/v1", cat.Handler()))
	mux.Handle("/media/upload/", http.StripPrefix("/media/upload", media.UploadHandler(uploads, media.UploadHandlerOptions{Auth: auth})))
	mux.Handle("/media/", http.StripPrefix("/media", reader.Handler(media.HandlerOptions{Auth: auth, Limit: media.RateLimit{Disabled: true}})))
	w, err := worker.New(ctx, worker.Config{Pool: pool, Schema: workers, ContentSchema: schema, Store: env.Store, Kinds: reg,
		HostSchema: hostRiver, TempDir: t.TempDir(), Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	h := mux
	staff, reader1, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "reader", Kind: "user"}, access.Actor{Anonymous: true, IP: "10.0.0.9"}
	call := func(actor access.Actor, method, target string, body, out any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := testauth.As(httptest.NewRequest(method, target, &buf).WithContext(ctx), actor)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if out != nil && rec.Code < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code
	}
	must := func(status int, actor access.Actor, method, target string, body, out any) {
		t.Helper()
		if code := call(actor, method, target, body, out); code != status {
			t.Fatalf("%s %s as %q: %d, want %d", method, target, actor.ID, code, status)
		}
	}
	put := func(ref media.RefBody, path string, seed uint8) string {
		t.Helper()
		body := pngImage(t, 640, 400, seed)
		sum := sha256.Sum256(body)
		var plan media.PresignReply
		must(http.StatusOK, staff, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: path, Type: "image/png",
			Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, &plan)
		req, _ := http.NewRequest(plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
		for k, v := range plan.Put.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		must(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
		return strings.TrimSuffix(plan.Path, ".png")
	}
	upload := func(kind, id string, seed uint8) string {
		return put(media.RefBody{Kind: kind, ID: id}, "photo.png", seed)
	}
	// exists reports a file a URL names in the bucket.
	exists := func(u string) bool {
		t.Helper()
		p, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		_, err = env.Store.Head(ctx, strings.TrimPrefix(p.Path, "/v1/"))
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}

	var created catalog.Content
	must(http.StatusCreated, staff, "POST", "/v1/contents", catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Pictures", Body: "soon"}}}, &created)
	id := created.ID
	// Body images live in the post's body item; its cover in its own folder.
	a, b, cover := upload("blog"+media.BodySuffix, id, 3), upload("blog"+media.BodySuffix, id, 9), upload("blog", id, 7)
	var inline catalog.InlineImage
	must(http.StatusOK, staff, "POST", "/v1/contents/"+id+"/images", content.ImageInput{Image: &a}, &inline)
	if inline.Ref != catalog.ImageRef(a) {
		t.Fatalf("inline image %+v", inline)
	}
	body := fmt.Sprintf(`<p><img src="%s"></p><p>and</p><p><img src="%s"></p>`, inline.Ref, catalog.ImageRef(b))
	must(http.StatusOK, staff, "PUT", "/v1/contents/"+id+"/text/en", catalog.Text{Title: "Pictures", Body: body}, nil)
	must(http.StatusOK, staff, "PATCH", "/v1/contents/"+id, catalog.ContentPatch{Cover: &cover}, nil)

	// A draft: its editors read references and see the images through
	// signed files; no one else sees the item or its folder.
	for _, actor := range []access.Actor{anon, reader1} {
		must(http.StatusNotFound, actor, "GET", "/v1/contents/"+id, nil, nil)
		must(http.StatusNotFound, actor, "GET", "/media/blog/"+id+"?editor", nil, nil)
	}
	var draft catalog.Content
	must(http.StatusOK, staff, "GET", "/v1/contents/"+id, nil, &draft)
	if draft.Text["en"].Body != body || draft.Cover == nil || draft.Cover.URL != nil || draft.Cover.Image != cover {
		t.Fatalf("draft %+v", draft)
	}
	for name, u := range draft.Images {
		if (name != a && name != b) || !strings.Contains(u, "?t=") || strings.Contains(u, "/public/") {
			t.Fatalf("draft image %s at %s, want its editors' signed file", name, u)
		}
	}
	eventually(t, "the editor preview", time.Minute, func() bool {
		must(http.StatusOK, staff, "POST", "/v1/contents/"+id+"/images", content.ImageInput{Image: &a}, &inline)
		return inline.URL != nil && strings.Contains(*inline.URL, "?t=") && !strings.Contains(*inline.URL, "/public/") && exists(*inline.URL)
	})

	// Published, every reference resolves to a public file that exists
	// (until it renders, a body image is the signed open rendition); each new
	// generation resolves to its own.
	resolved := func(what string, before map[string]string) catalog.Content {
		t.Helper()
		var c catalog.Content
		eventually(t, what, 2*time.Minute, func() bool {
			c = catalog.Content{}
			if call(anon, "GET", "/v1/contents/"+id, nil, &c) != http.StatusOK || strings.Contains(c.Body, catalog.ImageScheme) || len(c.Images) != 3 {
				return false
			}
			for name, u := range c.Images {
				if u == before[name] || !strings.Contains(u, "/public/") || !exists(u) {
					return false
				}
			}
			return true
		})
		if want := fmt.Sprintf(`<p><img src="%s"></p><p>and</p><p><img src="%s"></p>`, c.Images[a], c.Images[b]); c.Body != want || c.Cover == nil || c.Cover.URL == nil || *c.Cover.URL != c.Images[cover] {
			t.Fatalf("%s: %+v", what, c)
		}
		var stored string
		if err := pool.QueryRow(ctx, `SELECT body FROM `+schema+`.content_text WHERE content_id = $1 AND language = 'en'`, id).Scan(&stored); err != nil || stored != body {
			t.Fatalf("stored body %q, err=%v", stored, err)
		}
		return c
	}
	must(http.StatusOK, staff, "POST", "/v1/contents/"+id+"/publish", nil, nil)
	published := resolved("the published images", nil)

	for _, kind := range []string{"blog", "blog" + media.BodySuffix} {
		ref, err := reg.Ref(kind, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := queue.Enqueue(ctx, media.ProcessJob{Ref: ref, Preset: "inline", Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	regenerated := resolved("the regenerated images", published.Images)
	eventually(t, "the body images' first generation's retirement", 2*time.Minute, func() bool {
		return !exists(published.Images[a]) && !exists(published.Images[b])
	})
	eventually(t, "the cover's first generation's retirement", 2*time.Minute, func() bool { return !exists(published.Images[cover]) })

	must(http.StatusOK, staff, "POST", "/v1/contents/"+id+"/unpublish", nil, nil)
	must(http.StatusNotFound, anon, "GET", "/v1/contents/"+id, nil, nil)
	eventually(t, "the unpublished item's references", time.Minute, func() bool {
		var hidden catalog.Content
		must(http.StatusOK, staff, "GET", "/v1/contents/"+id, nil, &hidden)
		for _, u := range hidden.Images {
			if strings.Contains(u, "/public/") {
				return false
			}
		}
		return hidden.Text["en"].Body == body && hidden.Cover != nil && hidden.Cover.URL == nil
	})
	must(http.StatusOK, staff, "POST", "/v1/contents/"+id+"/publish", nil, nil)
	resolved("the images shown again", regenerated.Images)

	// Poll images store names and resolve the same way.
	var poll content.Poll
	must(http.StatusCreated, staff, "POST", "/polls", content.PollInput{Question: "Which?", Language: "en",
		Options: []content.PollOptionInput{{Label: "A"}, {Label: "B", Position: 1}}}, &poll)
	q := upload("poll", poll.ID, 5)
	must(http.StatusOK, staff, "PUT", "/polls/"+poll.ID+"/image", content.ImageInput{Image: &q}, nil)
	var first content.Poll
	eventually(t, "the poll image", time.Minute, func() bool {
		must(http.StatusOK, anon, "GET", "/polls/"+poll.ID, nil, &first)
		return first.ImageURL != "" && exists(first.ImageURL)
	})
	pollRef, _ := reg.Ref("poll", poll.ID)
	if err := queue.Enqueue(ctx, media.ProcessJob{Ref: pollRef, Preset: "inline", Force: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the regenerated poll image", time.Minute, func() bool {
		var p content.Poll
		must(http.StatusOK, anon, "GET", "/polls/"+poll.ID, nil, &p)
		return p.ImageURL != "" && p.ImageURL != first.ImageURL && exists(p.ImageURL)
	})

	// Version media follows catalog writes: a new version forks a sibling's
	// pages, and purging a deleted item deletes its media.
	var g catalog.Content
	must(http.StatusCreated, staff, "POST", "/v1/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Pages"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &g)
	from := g.Versions[0].ID
	put(media.RefBody{Kind: "gallery", ID: g.ID, Version: from}, "pages/001.png", 11)
	var forked catalog.Version
	must(http.StatusCreated, staff, "POST", "/v1/contents/"+g.ID+"/versions", catalog.VersionInput{Languages: []string{"ja"},
		Fork: &catalog.VersionFork{From: from}}, &forked)
	forkRef, err := reg.VersionRef("gallery", g.ID, forked.ID)
	if err != nil {
		t.Fatal(err)
	}
	pages := func() bool {
		m, _, err := jobs.Manifests().Get(ctx, forkRef)
		if err != nil || m == nil {
			return false
		}
		_, ok := m.Get("pages/001.png")
		return ok
	}
	eventually(t, "the forked pages", time.Minute, pages)
	// A rule change republishes the versions that inherit it.
	must(http.StatusOK, staff, "PATCH", "/v1/contents/"+g.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.PPV}}, nil)
	must(http.StatusNoContent, staff, "DELETE", "/v1/contents/"+g.ID, nil, nil)
	if n, err := cat.PurgeDeleted(ctx); err != nil || n != 1 {
		t.Fatalf("PurgeDeleted %d %v", n, err)
	}
	eventually(t, "the purged media", time.Minute, func() bool { return !pages() })
}
