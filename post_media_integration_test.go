package contentkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
)

// contentFolders routes the post and poll media kinds to the content runtime,
// as a host's registry hooks do; it is bound once the runtime exists.
type contentFolders struct{ rt *content.Runtime }

func (c *contentFolders) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	return c.rt.MediaResolver().Resolve(ctx, refs, a)
}

func (c *contentFolders) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	return c.rt.CanUpload(ctx, a, t)
}

// A draft post's images are private until it is published: its editors read
// them through the media read path, everyone else is refused, and publishing
// shows the folder to anonymous viewers.
func TestDraftPostImagesReadForEditorsIntegration(t *testing.T) {
	if os.Getenv("CONTENTKIT_TEST_S3_ENDPOINT") == "" {
		t.Skip("needs CONTENTKIT_TEST_S3_*")
	}
	ctx := context.Background()
	pool := testPG(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	env := mediatest.Open(t)
	schema := env.ContentSchema() // the media journal's schema, ContentKit's baseline applied
	images := []string{"image/png"}
	folder := func(name string) media.Kind {
		return media.Kind{Name: name, Uploads: []media.Upload{{Path: "{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 10}},
			Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 800}}}}
	}
	hooks := &contentFolders{}
	reg, err := media.NewRegistry(media.Config{Namespace: env.Tenant, BaseURL: "https://media.test", Kinds: []media.Kind{folder("post"), folder("poll")},
		Hooks: media.Hooks{Resolver: hooks, CanUpload: hooks}})
	if err != nil {
		t.Fatal(err)
	}
	journal, queue := env.Processing(reg)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: reg, Locker: mediatest.Locker(t, env.Store), Journal: journal, Processes: queue, Pool: env.Pool()})
	if err != nil {
		t.Fatal(err)
	}
	// Exposure jobs queue in the content transaction; no worker runs them here.
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema}, jobs.RiverJobs()); err != nil {
		t.Fatal(err)
	}
	key := token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}
	ring, err := token.NewRing(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: jobs.Manifests(), Tickets: &ring, Commits: media.RateLimit{Disabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: jobs.Manifests(), Queue: queue, Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: key}})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(ctx, RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: env.Tenant},
		Content: content.Options{Identity: ctxIdentity{}, Authz: staffAuthz{}, Resolver: itemResolver{}, ContentKinds: []string{"gallery"},
			Limits: content.Limits{Disabled: true}, Media: &content.Media{Images: jobs.Manifests(), Folders: jobs},
			Perms: content.Perms{PostWrite: "post", PollWrite: "poll"}},
		Uploads: uploads, Reader: reader, ReadLimit: media.RateLimit{Disabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	hooks.rt = rt.Content
	h := rt.Handler()
	call := func(status int, actor access.Actor, method, target string, body, out any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, target, &buf)
		req = req.WithContext(context.WithValue(req.Context(), actorKey{}, actor))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s as %q: %d, want %d: %s", method, target, actor.ID, rec.Code, status, rec.Body)
		}
		if out != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true, IP: "10.0.0.9"}

	var post content.Post
	call(http.StatusCreated, staff, "POST", "/posts", content.PostInput{Title: ptr("Draft"), Body: ptr("text"), IsDraft: ptr(true)}, &post)
	ref := media.RefBody{Kind: "post", ID: post.ID}
	body := pngBytes(t, 16)
	sum := sha256.Sum256(body)
	var plan media.PresignReply
	call(http.StatusOK, staff, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: "photo.png", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, &plan)
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
	call(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	name := strings.TrimSuffix(plan.Path, ".png")
	// No worker runs here: the draft's image has neither a public file nor an
	// editor view yet, so the editor gets only its reference.
	var inline content.InlineImage
	call(http.StatusOK, staff, "POST", "/posts/"+post.ID+"/images", content.ImageInput{Image: &name}, &inline)
	if inline.Ref != content.ImageRef(name) || inline.URL != nil {
		t.Fatalf("inline image %+v", inline)
	}
	call(http.StatusOK, staff, "PUT", "/posts/"+post.ID+"/cover", content.ImageInput{Image: &name}, nil)

	// The draft's editor reads its images; no one else sees the folder.
	var read media.ReadResult
	call(http.StatusOK, staff, "GET", "/media/post/"+post.ID+"?editor", nil, &read)
	if read.Access != media.AccessFull || len(read.Files) != 1 || read.Files[0].Path != plan.Path || !read.Files[0].Upload || len(read.Public) != 0 {
		t.Fatalf("editor read of the draft %+v", read)
	}
	call(http.StatusNotFound, anon, "GET", "/media/post/"+post.ID, nil, nil)
	call(http.StatusNotFound, user, "GET", "/media/post/"+post.ID+"?editor", nil, nil)

	// Published, the folder shows to everyone; deleted, to no one.
	call(http.StatusOK, staff, "PATCH", "/posts/"+post.ID, content.PostInput{IsDraft: ptr(false)}, nil)
	var viewer media.ReadResult
	call(http.StatusOK, anon, "GET", "/media/post/"+post.ID, nil, &viewer)
	if viewer.Access != media.AccessFull || viewer.Uploads != nil {
		t.Fatalf("anonymous read of the published post %+v", viewer)
	}
	call(http.StatusNoContent, staff, "DELETE", "/posts/"+post.ID, nil, nil)
	call(http.StatusNotFound, staff, "GET", "/media/post/"+post.ID+"?editor", nil, nil)
}
