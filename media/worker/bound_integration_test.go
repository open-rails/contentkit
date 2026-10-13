package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/worker"
)

// An avatar written through an engine that binds the accounts namespace to
// its owner (contentkit#131) is rendered by the owner's stock worker, from
// the owner's queue, journal and bucket, and both engines serve the same
// published files.
func TestBoundNamespaceRendersOnTheOwnersWorker(t *testing.T) {
	ownerEnv, otherEnv := s3test.Open(t), s3test.Open(t)
	if !ownerEnv.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	schema := func() string {
		s := "ck_mb_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
		t.Cleanup(func() {
			for _, name := range []string{s, s + "_river", s + "_media_worker"} {
				_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE")
			}
			_, _ = pool.Exec(context.Background(), "DELETE FROM public.migrations WHERE schema = $1", s)
		})
		return s
	}
	ownerSchema, otherSchema := schema(), schema()
	key := "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))
	config := func(env *s3test.Env, schema string) contentkit.Config {
		return contentkit.Config{Tenant: env.Tenant, Database: contentkit.DatabaseConfig{Schema: schema},
			Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{
				"blog": {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}}},
			Interactions: contentkit.InteractionsConfig{Limits: contentkit.LimitsConfig{Disabled: true}},
			Media: &contentkit.MediaConfig{Config: media.Config{BaseURL: "https://media." + env.Tenant + ".test",
				Kinds: []media.Kind{avatar.User()}, Avatars: media.Avatars{Kind: "user", Preset: "avatar"}},
				Delivery:    contentkit.DeliveryConfig{SigningKey: key},
				CommitLimit: contentkit.RateLimitConfig{Disabled: true}, ReadLimit: contentkit.RateLimitConfig{Disabled: true}}}
	}
	start := func(cfg contentkit.Config, env *s3test.Env) http.Handler {
		e, err := contentkit.New(ctx, cfg, contentkit.Deps{Postgres: pool, Auth: testauth.Auth{Authz: ckStaff{}}, Store: env.Store})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.Background()) })
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		routes, err := e.Routes(contentkit.Routes{Prefix: "/api/ck", Groups: contentkit.UploadRoutes | contentkit.MediaRoutes})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		for _, r := range routes {
			pattern := r.Path
			if r.Method != "" {
				pattern = r.Method + " " + r.Path
			}
			mux.Handle(pattern, r.Handler)
		}
		return mux
	}
	ownerCfg := config(ownerEnv, ownerSchema)
	viaOwner := start(ownerCfg, ownerEnv)
	otherCfg := config(otherEnv, otherSchema)
	otherCfg.Media.Bind = map[string]contentkit.NamespaceOwner{"accounts": {Schema: ownerSchema, Bucket: ownerEnv.Config.Bucket}}
	viaOther := start(otherCfg, otherEnv)

	// The owner's stock worker: its registry, worker schema, journal and bucket.
	mc, err := ownerCfg.MediaKinds()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := media.NewRegistry(mc)
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(ctx, worker.Config{Pool: pool, Schema: ownerSchema + "_media_worker", ContentSchema: ownerSchema,
		Store: ownerEnv.Store, Kinds: reg, HostSchema: ownerSchema + "_river", TempDir: t.TempDir(), Threads: 2})
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

	user := access.Actor{ID: uuid.Must(uuid.NewV7()).String(), Kind: "user"}
	call := func(h http.Handler, method, target string, body, out any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, testauth.As(httptest.NewRequest(method, target, &buf).WithContext(ctx), user))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, target, rec.Code, rec.Body)
		}
		if out != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	ref := media.RefBody{Kind: "user", ID: user.ID}
	body := pngImage(t, 600, 600, 5)
	sum := sha256.Sum256(body)
	var plan media.PresignReply
	call(viaOther, "POST", "/api/ck/v1/media/upload/presign", media.PresignBody{Ref: ref, Path: "avatar.png", Type: "image/png",
		Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, &plan)
	req, _ := http.NewRequestWithContext(ctx, plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
	for k, v := range plan.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	call(viaOther, "POST", "/api/ck/v1/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)

	// published lists the avatar's public files as read through h.
	published := func(h http.Handler) []string {
		var r media.ReadResult
		call(h, "GET", "/api/ck/v1/media/user/"+user.ID, nil, &r)
		var out []string
		for _, img := range r.Public {
			for _, rd := range img.Renditions {
				u, err := url.Parse(rd.URL)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, strings.TrimPrefix(u.Path, "/v1/"))
			}
		}
		slices.Sort(out)
		return out
	}
	var files []string
	eventually(t, "the owner's worker rendering the avatar", 2*time.Minute, func() bool {
		files = published(viaOther)
		return len(files) > 0
	})
	if theirs := published(viaOwner); !slices.Equal(files, theirs) {
		t.Fatalf("the binding engine serves %v, the owner %v", files, theirs)
	}
	for _, key := range files {
		if _, err := ownerEnv.Store.Head(ctx, key); err != nil {
			t.Fatalf("%s is not in the owner's bucket: %v", key, err)
		}
		if _, err := otherEnv.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("%s reached the binding engine's bucket: %v", key, err)
		}
	}
}
