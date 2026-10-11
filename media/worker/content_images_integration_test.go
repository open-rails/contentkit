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

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
)

type ckActor struct{}

type ckIdentity struct{}

func (ckIdentity) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(ckActor{}).(access.Actor)
	return a, ok
}

// ckStaff holds every content permission for "staff" only.
type ckStaff struct{}

func (ckStaff) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff" && !a.Anonymous, nil
}

type noItems struct{}

func (noItems) Resolve(context.Context, []contentref.ContentRef, access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	return map[contentref.ContentKey]access.Resolution{}, nil
}

// contentFolders routes the post and poll kinds to the content module, as a
// host's registry hooks do.
type contentFolders struct{ rt *content.Runtime }

func (c *contentFolders) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	return c.rt.MediaResolver().Resolve(ctx, refs, a)
}

func (c *contentFolders) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	return c.rt.CanUpload(ctx, a, t)
}

type keepHTML struct{}

func (keepHTML) Sanitize(_ context.Context, raw string) (string, error) { return raw, nil }

// A post body stores references to its images, so every image it shows
// resolves to the current file through publish, a forced regenerate and a
// hide and show (each a new generation, the old one retired), with the real
// worker rendering. A draft's images are its editors' alone.
func TestPostImagesResolveAcrossGenerations(t *testing.T) {
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
	hooks := &contentFolders{}
	reg, err := media.NewRegistry(media.Config{Namespace: env.Tenant, BaseURL: "https://media.example",
		Kinds: []media.Kind{folder("post"), folder("poll")}, Hooks: media.Hooks{Resolver: hooks, CanUpload: hooks}})
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
	client, err := riverhelpers.New(ctx, pool, &river.Config{Schema: hostRiver, FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond}, jobs.RiverJobs())
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
	rt, err := contentkit.NewRuntime(ctx, contentkit.RuntimeConfig{
		EmbeddedConfig: contentkit.EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: env.Tenant},
		Content: content.Options{Identity: ckIdentity{}, Authz: ckStaff{}, Resolver: noItems{}, ContentKinds: []string{"gallery"},
			PostBodyProcessor: keepHTML{}, Limits: content.Limits{Disabled: true},
			Media: &content.Media{Images: jobs.Manifests(), Folders: jobs}, Perms: content.Perms{PostWrite: "post", PollWrite: "poll"}},
		Uploads: uploads, Reader: reader, ReadLimit: media.RateLimit{Disabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	hooks.rt = rt.Content
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

	h := rt.Handler()
	staff, reader1, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "reader", Kind: "user"}, access.Actor{Anonymous: true, IP: "10.0.0.9"}
	call := func(actor access.Actor, method, target string, body, out any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, target, &buf).WithContext(context.WithValue(ctx, ckActor{}, actor))
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
	upload := func(kind, id string, seed uint8) string {
		t.Helper()
		body := pngImage(t, 640, 400, seed)
		sum := sha256.Sum256(body)
		ref := media.RefBody{Kind: kind, ID: id}
		var plan media.PresignReply
		must(http.StatusOK, staff, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: "photo.png", Type: "image/png",
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

	var post content.Post
	must(http.StatusCreated, staff, "POST", "/posts", content.PostInput{Title: ptrTo("Pictures"), Body: ptrTo("soon"), IsDraft: ptrTo(true)}, &post)
	id := post.ID
	a, b := upload("post", id, 3), upload("post", id, 9)
	var inline content.InlineImage
	must(http.StatusOK, staff, "POST", "/posts/"+id+"/images", content.ImageInput{Image: &a}, &inline)
	if inline.Ref != content.ImageRef(a) {
		t.Fatalf("inline image %+v", inline)
	}
	body := fmt.Sprintf(`<p><img src="%s"></p><p>and</p><p><img src="%s"></p>`, inline.Ref, content.ImageRef(b))
	must(http.StatusOK, staff, "PATCH", "/posts/"+id, content.PostInput{Body: &body}, nil)
	must(http.StatusOK, staff, "PUT", "/posts/"+id+"/cover", content.ImageInput{Image: &b}, nil)

	// A draft: its editors read references and see the images through
	// signed editor views; no one else sees the post or its folder.
	for _, actor := range []access.Actor{anon, reader1} {
		must(http.StatusNotFound, actor, "GET", "/posts/"+id, nil, nil)
		must(http.StatusNotFound, actor, "GET", "/media/post/"+id+"?editor", nil, nil)
	}
	var draft content.Post
	must(http.StatusOK, staff, "GET", "/posts/"+id, nil, &draft)
	if draft.Body != body || draft.Images != nil || draft.CoverURL != nil || *draft.Cover != b {
		t.Fatalf("draft %+v", draft)
	}
	eventually(t, "the editor preview", time.Minute, func() bool {
		must(http.StatusOK, staff, "POST", "/posts/"+id+"/images", content.ImageInput{Image: &a}, &inline)
		return inline.URL != nil && strings.Contains(*inline.URL, "?t=") && !strings.Contains(*inline.URL, "/public/") && exists(*inline.URL)
	})

	// Published, every reference resolves to a file that exists; each new
	// generation resolves to its own.
	resolved := func(what string, before map[string]string) content.Post {
		t.Helper()
		var p content.Post
		eventually(t, what, 2*time.Minute, func() bool {
			p = content.Post{}
			if call(anon, "GET", "/posts/"+id, nil, &p) != http.StatusOK || strings.Contains(p.Body, content.ImageScheme) || len(p.Images) != 2 {
				return false
			}
			for name, u := range p.Images {
				if u == before[name] || !exists(u) {
					return false
				}
			}
			return true
		})
		if want := fmt.Sprintf(`<p><img src="%s"></p><p>and</p><p><img src="%s"></p>`, p.Images[a], p.Images[b]); p.Body != want || p.CoverURL == nil || *p.CoverURL != p.Images[b] {
			t.Fatalf("%s: %+v", what, p)
		}
		var stored string
		if err := pool.QueryRow(ctx, `SELECT body FROM `+schema+`.content_posts WHERE id = $1`, id).Scan(&stored); err != nil || stored != body {
			t.Fatalf("stored body %q, err=%v", stored, err)
		}
		return p
	}
	must(http.StatusOK, staff, "PATCH", "/posts/"+id, content.PostInput{IsDraft: ptrTo(false)}, nil)
	published := resolved("the published images", nil)
	var listed []content.Post
	must(http.StatusOK, anon, "GET", "/posts", nil, &listed)
	if len(listed) != 1 || listed[0].Body != published.Body {
		t.Fatalf("listed %+v", listed)
	}

	ref, err := reg.Ref("post", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, media.ProcessJob{Ref: ref, Preset: "inline", Force: true}); err != nil {
		t.Fatal(err)
	}
	regenerated := resolved("the regenerated images", published.Images)
	eventually(t, "the first generation's retirement", 2*time.Minute, func() bool {
		return !exists(published.Images[a]) && !exists(published.Images[b])
	})

	must(http.StatusOK, staff, "PATCH", "/posts/"+id, content.PostInput{IsDraft: ptrTo(true)}, nil)
	must(http.StatusNotFound, anon, "GET", "/posts/"+id, nil, nil)
	eventually(t, "the hidden post's references", time.Minute, func() bool {
		var hidden content.Post
		must(http.StatusOK, staff, "GET", "/posts/"+id, nil, &hidden)
		return hidden.Body == body && hidden.Images == nil && hidden.CoverURL == nil
	})
	must(http.StatusOK, staff, "PATCH", "/posts/"+id, content.PostInput{IsDraft: ptrTo(false)}, nil)
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
}

func ptrTo[T any](v T) *T { return &v }
