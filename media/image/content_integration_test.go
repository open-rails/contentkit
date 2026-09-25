package image_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/migrations"
)

type actorKey struct{}

type ctxIdentity struct{}

func (ctxIdentity) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(access.Actor)
	return a, ok
}

// editors hold every permission; everyone else holds none.
type editors struct{}

func (editors) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "editor", nil
}

type noContent struct{}

func (noContent) Resolve(context.Context, []contentref.ContentRef, access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	return nil, nil
}

// contentEnv is a host: content and media mounted behind one auth
// middleware, River running media's jobs with the libvips processor, MinIO.
type contentEnv struct {
	*s3test.Env
	srv       *httptest.Server
	urls      map[string]string // inline image name → its rendered URL
	originals map[string]string // inline image name → its original's key
}

func newContentEnv(t *testing.T) *contentEnv {
	t.Helper()
	s := s3test.Open(t)
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}

	inline := media.Spec{Width: 64, Fit: media.FitInside, Quality: 80}
	types := []string{"image/png", "image/jpeg"}
	kinds, err := media.NewRegistry(
		media.Kind{Name: "post", Types: types, MaxBytes: 10 << 20, Inline: &inline},
		media.Kind{Name: "poll", Types: types, MaxBytes: 10 << 20, Inline: &inline},
	)
	if err != nil {
		t.Fatal(err)
	}
	manifests := s3test.Manifests(t, s.Store, kinds, media.ManifestOptions{})
	jobs, err := media.NewJobs(media.JobsConfig{Store: s.Store, Locker: s3test.Locker(t, s.Store), Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	proc, err := image.New(image.Config{Store: s.Store, Kinds: kinds, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: manifests, Kinds: kinds, Resolver: noContent{},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: "https://media.test",
			SigningKey: token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := content.New(ctx, content.Options{Pool: pool, Schema: schema, Tenant: s.Tenant, Identity: ctxIdentity{}, Authz: editors{},
		Resolver: noContent{}, Perms: content.Perms{PostWrite: "post:write", PollWrite: "poll:write"},
		Media: &content.Media{URLs: reader, Folders: jobs}})
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := media.NewUploads(media.UploadOptions{Store: s.Store, Kinds: kinds, Manifests: manifests, Authorizer: rt, Queue: processNow{proc}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema, FetchPollInterval: 100 * time.Millisecond,
		FetchCooldown: 50 * time.Millisecond}, jobs.RiverJobs())
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

	actor := func(r *http.Request) (access.Actor, bool) {
		id := r.Header.Get("X-Actor")
		return access.Actor{ID: id, Kind: "user"}, id != ""
	}
	mux := http.NewServeMux()
	mux.Handle("/upload/", http.StripPrefix("/upload", media.UploadHandler(uploads, media.UploadHandlerOptions{Tenant: s.Tenant, Actor: actor, Reader: reader})))
	mux.Handle("/api/", http.StripPrefix("/api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, _ := actor(r)
		rt.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, a)))
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &contentEnv{Env: s, srv: srv, urls: map[string]string{}, originals: map[string]string{}}
}

func (e *contentEnv) call(t *testing.T, actor, method, path string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("X-Actor", actor)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 && out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// uploadInline plays the browser SDK's uploadInline: presign, PUT to the
// bucket, commit-slot (rendered at once here), recording its URL. It
// returns the image name, or "" with the refusal.
func (e *contentEnv) uploadInline(t *testing.T, actor string, ref media.RefBody, body []byte) (string, int) {
	t.Helper()
	sum := sha256.Sum256(body)
	var p media.PresignReply
	if code := e.call(t, actor, "POST", "/upload/presign", media.PresignBody{Ref: ref, Type: "image/png", Size: int64(len(body)),
		SHA256: hex.EncodeToString(sum[:]), Inline: true}, &p); code != 200 {
		return "", code
	}
	req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
	for k, v := range p.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	var m media.SlotManifest
	if code := e.call(t, actor, "POST", "/upload/commit-slot", media.SlotBody{Ref: ref, Slot: p.Name, SHA256: hex.EncodeToString(sum[:])}, &m); code != 200 ||
		m.Pending || len(m.Outputs) != 1 {
		t.Fatalf("commit-slot: %d %+v", code, m)
	}
	e.urls[p.Name] = m.Outputs[0].URL
	e.originals[p.Name] = e.Tenant + "/" + ref.Kind + "/" + ref.ID + "/originals/" + media.SHA256Name(sum[:])
	return p.Name, 200
}

// public waits for the image job to write key and returns its bytes.
func (e *contentEnv) public(t *testing.T, key string) []byte {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		if rc, obj, err := e.Store.Get(context.Background(), key, media.GetOptions{}); err == nil {
			defer rc.Close()
			var b bytes.Buffer
			_, _ = b.ReadFrom(rc)
			if obj.ContentType != "image/webp" || obj.CacheControl != "max-age=31536000, immutable" {
				t.Fatalf("%s: %+v", key, obj)
			}
			return b.Bytes()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written", key)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestContentImages runs post inline/cover images and poll option images the
// way a host serves them: SDK-shaped uploads through media's handler
// authorized by content, the libvips job on River, URLs stored by content,
// and the folder deleted with the post.
func TestContentImages(t *testing.T) {
	e := newContentEnv(t)
	ctx := context.Background()
	url := func(key string) string { return "https://media.test/" + key }

	var post struct{ ID string }
	if code := e.call(t, "editor", "POST", "/api/posts", map[string]any{"title": "Hello", "body": "<p>hi</p>"}, &post); code != 201 {
		t.Fatalf("create post: %d", code)
	}
	postRef := media.RefBody{Kind: "post", ID: post.ID}
	if _, code := e.uploadInline(t, "reader", postRef, pngImage(t, 300, 400, 3)); code != 403 {
		t.Fatalf("non-editor upload: %d", code)
	}
	if _, code := e.uploadInline(t, "editor", media.RefBody{Kind: "post", ID: contentref.NewID()}, pngImage(t, 30, 40, 3)); code != 403 {
		t.Fatalf("upload to a missing post: %d", code)
	}

	name, _ := e.uploadInline(t, "editor", postRef, pngImage(t, 300, 400, 4))
	folder := e.Tenant + "/post/" + post.ID + "/"
	var inline struct{ URL string }
	if code := e.call(t, "editor", "POST", "/api/posts/"+post.ID+"/images", map[string]string{"image": name}, &inline); code != 200 ||
		inline.URL != e.urls[name] || !strings.HasPrefix(inline.URL, url(folder+"public/sha256-")) {
		t.Fatalf("inline url %d %q", code, inline.URL)
	}
	if w, h := webpSize(t, e.public(t, strings.TrimPrefix(inline.URL, url("")))); w != 64 || h < 85 || h > 86 {
		t.Fatalf("inline image %dx%d", w, h)
	}
	if code := e.call(t, "editor", "POST", "/api/posts/"+post.ID+"/images", map[string]string{"image": media.NewInlineName()}, nil); code != 400 {
		t.Fatalf("unrendered inline image: %d", code)
	}
	if obj, err := e.Store.Head(ctx, e.originals[name]); err != nil || obj.ContentType != "image/png" {
		t.Fatalf("original kept private in originals/: %+v %v", obj, err)
	}

	cover, _ := e.uploadInline(t, "editor", postRef, pngImage(t, 200, 100, 5))
	var set struct {
		CoverURL string `json:"cover_url"`
	}
	if code := e.call(t, "editor", "PUT", "/api/posts/"+post.ID+"/cover", map[string]string{"image": cover}, &set); code != 200 ||
		set.CoverURL != e.urls[cover] {
		t.Fatalf("cover %d %q", code, set.CoverURL)
	}
	var got struct {
		CoverURL string `json:"cover_url"`
	}
	e.call(t, "editor", "GET", "/api/posts/"+post.ID, nil, &got)
	if got.CoverURL != set.CoverURL {
		t.Fatalf("stored cover %q", got.CoverURL)
	}
	e.public(t, strings.TrimPrefix(set.CoverURL, url("")))

	var poll struct {
		ID      string
		Options []struct{ ID string }
	}
	if code := e.call(t, "editor", "POST", "/api/polls", map[string]any{"question": "Best?", "options": []map[string]string{{"label": "A"}, {"label": "B"}}}, &poll); code != 201 {
		t.Fatalf("create poll: %d", code)
	}
	option, _ := e.uploadInline(t, "editor", media.RefBody{Kind: "poll", ID: poll.ID}, pngImage(t, 128, 128, 6))
	var img struct {
		ImageURL string `json:"image_url"`
	}
	pollFolder := e.Tenant + "/poll/" + poll.ID + "/"
	if code := e.call(t, "editor", "PUT", "/api/polls/"+poll.ID+"/options/"+poll.Options[1].ID+"/image", map[string]string{"image": option}, &img); code != 200 ||
		img.ImageURL != e.urls[option] {
		t.Fatalf("option image %d %q", code, img.ImageURL)
	}
	optionKey := strings.TrimPrefix(img.ImageURL, url(""))
	if w, h := webpSize(t, e.public(t, optionKey)); w != 64 || h != 64 || !strings.HasPrefix(optionKey, pollFolder+"public/") {
		t.Fatalf("option image %dx%d", w, h)
	}
	var view struct {
		Options []struct {
			ID       string
			ImageURL string `json:"image_url"`
		}
	}
	e.call(t, "editor", "GET", "/api/polls/"+poll.ID, nil, &view)
	for _, o := range view.Options {
		if (o.ID == poll.Options[1].ID) != (o.ImageURL == img.ImageURL) {
			t.Fatalf("poll options %+v", view.Options)
		}
	}

	if code := e.call(t, "editor", "DELETE", "/api/posts/"+post.ID, nil, nil); code != 200 {
		t.Fatalf("delete post: %d", code)
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		left := 0
		for _, err := range e.Store.List(ctx, folder) {
			if err != nil {
				t.Fatal(err)
			}
			left++
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d objects left in %s", left, folder)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := e.Store.Head(ctx, optionKey); err != nil {
		t.Fatalf("the poll's folder must stay: %v", err)
	}
}

// processNow processes a job as it is enqueued, as the media worker would.
type processNow struct{ p *image.Processor }

func (q processNow) Enqueue(ctx context.Context, j media.ProcessJob) error {
	return q.p.Process(ctx, j)
}
