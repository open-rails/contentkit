package contentkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/signal"
)

// Two engines share one database and the avatars' accounts namespace, which
// the second binds to the first (contentkit#131). Booted before its owner it
// degrades; an avatar it then writes commits in the owner's journal, lands
// in the owner's bucket, queues on the owner's worker, is placed by the
// owner and reads the same through both; erasing the person through it
// erases the avatar in the owner. Nothing reaches its own journal, bucket
// or worker queue.
func TestEngineBoundNamespaceIntegration(t *testing.T) {
	ctx := t.Context()
	ownerEnv, otherEnv := mediatest.Open(t), mediatest.Open(t)
	pool := pgtest.Pool(t, nil)
	ownerSchema, otherSchema := engineSchema(t, pool), engineSchema(t, pool)
	key := "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))
	config := func(env *mediatest.Env, schema string) Config {
		cfg := siteConfig(schema)
		cfg.Tenant = env.Tenant
		cfg.Media = &MediaConfig{Config: media.Config{BaseURL: "https://media." + env.Tenant + ".test", Kinds: []media.Kind{avatar.User()},
			Avatars: media.Avatars{Kind: "user", Preset: "avatar"}},
			Delivery: DeliveryConfig{SigningKey: key}, CommitLimit: RateLimitConfig{Disabled: true}, ReadLimit: RateLimitConfig{Disabled: true}}
		return cfg
	}
	deps := func(env *mediatest.Env) Deps {
		return Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Store: env.Store}
	}
	start := func(cfg Config, d Deps) *Engine {
		e, err := New(ctx, cfg, d)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.WithoutCancel(ctx)) })
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return e
	}
	probe := func(e *Engine) error {
		for _, p := range e.Probes() {
			if p.Name == "contentkit_media_owner_accounts" {
				return p.Check(ctx)
			}
		}
		t.Fatalf("no owner probe in %+v", e.Probes())
		return nil
	}

	otherCfg := config(otherEnv, otherSchema)
	bind := func(o NamespaceOwner) Config {
		c := otherCfg
		m := *c.Media
		m.Bind = map[string]NamespaceOwner{"accounts": o}
		c.Media = &m
		return c
	}
	for _, o := range []NamespaceOwner{{Schema: ownerSchema}, {Schema: otherSchema, Bucket: ownerEnv.Config.Bucket},
		{Schema: "Not-A-Schema", Bucket: ownerEnv.Config.Bucket}} {
		if _, err := New(ctx, bind(o), deps(otherEnv)); err == nil {
			t.Fatalf("New took the binding %+v", o)
		}
	}
	unknown := bind(NamespaceOwner{Schema: ownerSchema, Bucket: ownerEnv.Config.Bucket})
	unknown.Media.Bind = map[string]NamespaceOwner{"elsewhere": unknown.Media.Bind["accounts"]}
	if _, err := New(ctx, unknown, deps(otherEnv)); err == nil {
		t.Fatal("New bound a namespace no kind lives in")
	}

	// The owner is not there yet: its namespace fails, nothing else does.
	other := start(bind(NamespaceOwner{Schema: ownerSchema, Bucket: ownerEnv.Config.Bucket}), deps(otherEnv))
	if err := probe(other); err == nil {
		t.Fatal("the owner probe passed before the owner migrated")
	}
	if err := other.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	owner := start(config(ownerEnv, ownerSchema), deps(ownerEnv))
	if err := probe(other); err != nil {
		t.Fatalf("owner probe once the owner runs: %v", err)
	}

	serveOf := func(e *Engine) server {
		routes, err := e.Routes(Routes{Prefix: "/api/ck", Groups: UploadRoutes | MediaRoutes})
		if err != nil {
			t.Fatal(err)
		}
		return serve(t, routes)
	}
	viaOwner, viaOther := serveOf(owner), serveOf(other)
	user := access.Actor{ID: uuid.Must(uuid.NewV7()).String(), Kind: "user"}
	ref := media.RefBody{Kind: "user", ID: user.ID}
	body := pngBytes(t, 32)
	sum := sha256.Sum256(body)
	var plan media.PresignReply
	viaOther.call(http.StatusOK, user, "POST", "/api/ck/v1/media/upload/presign",
		media.PresignBody{Ref: ref, Path: "avatar.png", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, &plan)
	put(t, plan.Put, body)
	viaOther.call(http.StatusOK, user, "POST", "/api/ck/v1/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)

	if n := rows(t, pool, ownerSchema, "content_media_commits", user.ID); n == 0 {
		t.Fatal("the avatar's commit is not in the owner's journal")
	}
	for _, table := range []string{"content_media_commits", "content_media_allocations", "content_media_publications"} {
		if n := rows(t, pool, otherSchema, table, ""); n != 0 {
			t.Fatalf("%d rows in the binding engine's %s", n, table)
		}
	}
	if n := rows(t, pool, ownerSchema+"_media_worker", "river_job", user.ID); n == 0 {
		t.Fatal("the avatar was not queued on the owner's worker")
	}
	objects := func(env *mediatest.Env) int {
		n := 0
		for _, err := range env.Store.List(ctx, "accounts/user/"+user.ID+"/") {
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		return n
	}
	if objects(ownerEnv) == 0 || objects(otherEnv) != 0 {
		t.Fatalf("objects: owner's bucket %d, binding engine's %d", objects(ownerEnv), objects(otherEnv))
	}

	read := func(s server) media.ReadResult {
		var r media.ReadResult
		s.call(http.StatusOK, user, "GET", "/api/ck/v1/media/user/"+user.ID+"?editor", nil, &r)
		return r
	}
	if a, b := read(viaOwner), read(viaOther); len(a.Files) != 1 || len(b.Files) != 1 || a.Files[0].Path != b.Files[0].Path || !b.Files[0].Staged {
		t.Fatalf("staged avatar reads: owner %+v, binding engine %+v", a.Files, b.Files)
	}
	// The owner's worker places it, as its place job does; an editor read
	// through the binding engine then asks the owner's worker for its view.
	mref, err := owner.media.reg.Ref("user", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := owner.media.jobs.Manifests().Place(ctx, mref); err != nil || n != 1 {
		t.Fatalf("owner placed %d: %v", n, err)
	}
	placed := read(viaOther)
	if len(placed.Files) != 1 || placed.Files[0].Staged {
		t.Fatalf("placed avatar read through the binding engine %+v", placed.Files)
	}
	if n := rows(t, pool, otherSchema+"_media_worker", "river_job", ""); n != 0 {
		t.Fatalf("%d jobs on the binding engine's worker", n)
	}
	var editor int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{ownerSchema + "_media_worker", "river_job"}.Sanitize()+`
WHERE kind = 'contentkit_media_image' AND args->'ref'->>'content_id' = $1 AND (args->>'editor')::bool`, user.ID).Scan(&editor); err != nil || editor == 0 {
		t.Fatalf("editor views queued on the owner's worker: %d (%v)", editor, err)
	}
	if a := read(viaOwner); len(a.Files) != 1 || a.Files[0].Path != placed.Files[0].Path || a.Files[0].Staged {
		t.Fatalf("placed avatar read through the owner %+v", a.Files)
	}

	report, err := other.EraseSubjects(ctx, []signal.Subject{{UserID: user.ID}})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure %+v %v", report, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _, err := owner.media.jobs.Manifests().Get(ctx, mref)
		if errors.Is(err, media.ErrNotFound) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the avatar outlived its person's erasure")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := rows(t, pool, otherSchema, "content_media_commits", ""); n != 0 {
		t.Fatalf("the erasure left %d commits in the binding engine's journal", n)
	}
	if objects(otherEnv) != 0 {
		t.Fatal("the erasure wrote to the binding engine's bucket")
	}
	for _, s := range []server{viaOwner, viaOther} {
		var r media.ReadResult
		rec := s.call(http.StatusOK, user, "GET", "/api/ck/v1/media/user/"+user.ID+"?editor", nil, &r)
		if len(r.Files) != 0 {
			t.Fatalf("erased avatar reads %s", rec.Body)
		}
	}
}

// rows counts schema.table's rows, of contentID's item when set.
func rows(t *testing.T, pool *pgxpool.Pool, schema, table, contentID string) int {
	t.Helper()
	q := `SELECT count(*) FROM ` + pgx.Identifier{schema, table}.Sanitize()
	var args []any
	if contentID != "" {
		col := "content_id"
		if table == "river_job" {
			col = "args->'ref'->>'content_id'"
		}
		q += ` WHERE ` + col + ` = $1`
		args = append(args, contentID)
	}
	var n int
	if err := pool.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// put sends a presigned upload as a browser does.
func put(t *testing.T, p *media.RequestReply, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode/100 != 2 {
		t.Fatalf("upload: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
}
