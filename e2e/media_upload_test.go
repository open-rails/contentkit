package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/worker"
)

type publisherTransport struct {
	key    string
	writes *atomic.Int32
}

func (p publisherTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+p.key)
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		p.writes.Add(1)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Exercise independent content creation, the ordinary three-step media
// upload, durable import after restart, status polling and explicit publication.
func TestMediaUploadSurvivesRestartWithoutPublishing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	env := mediatest.Open(t)
	schema := "ck_publish_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, s := range []string{schema, schema + "_river", schema + "_media_worker", schema + "_accounts"} {
			_, _ = pool.Exec(clean, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s}.Sanitize()+" CASCADE")
		}
		_, _ = pool.Exec(clean, "DELETE FROM public.migrations WHERE schema=$1", schema)
		for object, err := range env.Store.List(clean, "tenant/"+scope.TenantID.String()+"/") {
			if err != nil {
				t.Error(err)
				break
			}
			if err = env.Store.Delete(clean, object.Key); err != nil {
				t.Error(err)
			}
		}
	})
	roles := authkit.NewRoles()
	persona := roles.Persona("app", authkit.APIKeys)
	perms := persona.Declare(ckauthkit.AppPermission(access.ContentWrite), ckauthkit.AppPermission(access.EntitiesWrite))
	publisher := persona.Role("publisher", perms[0], perms[1])
	for _, permission := range access.Permissions() {
		if permission != access.ContentWrite && permission != access.EntitiesWrite {
			persona.Declare(ckauthkit.AppPermission(permission))
		}
	}
	nativeTenant, _ := iam.ParseTenantID(scope.TenantID.String())
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles; c.DefaultTenantID = nativeTenant }))
	appPersona, err := ak.Persona("app")
	if err != nil {
		t.Fatal(err)
	}
	app, err := ak.CreateGroup(ctx, iam.NewGroup{ID: scope.AppID.String(), TenantID: nativeTenant, Persona: appPersona})
	if err != nil {
		t.Fatal(err)
	}
	appRef := iam.GroupByID(app.ID)
	bound, err := ak.Scope(ctx, appRef)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := ckauthkit.NewAuthorization(ctx, ak, ckauthkit.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: bound})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ak.CreateAPIKey(ctx, iam.SystemIdentity(), appRef, iam.NewAPIKey{Name: "publishing integration", Role: publisher})
	if err != nil {
		t.Fatal(err)
	}
	cfg := contentkit.Config{Scope: &scope, LegacyCatalogKey: env.Tenant, Database: contentkit.DatabaseConfig{Schema: schema}, Accounts: &contentkit.AccountsConfig{Schema: schema + "_accounts"},
		Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Entities: []string{"artist", "series", "tag"}}}, EntityKinds: map[string]kinds.EntityKind{"artist": {Route: "artists"}, "series": {Route: "series"}, "tag": {Route: "tags"}}},
		Media: &contentkit.MediaConfig{Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "low", From: "originals/{name}", To: "low/{name}.webp", Image: &media.Image{Width: 64}}},
			Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{64}}},
			Publish: &media.Publish{Pages: "originals/{name}", Renditions: []string{"low"}, Import: "import/{name}"},
		}}}, Delivery: contentkit.DeliveryConfig{SigningKey: "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))}, CommitLimit: contentkit.RateLimitConfig{Disabled: true}, ReadLimit: contentkit.RateLimitConfig{Disabled: true}}}
	deps := contentkit.Deps{Postgres: pool, Store: env.Store, Auth: ak.Authenticator(), Authorization: policy}
	engine, err := contentkit.New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	if err = engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	serveEngine := func(e *contentkit.Engine) *httptest.Server {
		s := httptest.NewServer(serve(t, e).h)
		t.Cleanup(s.Close)
		return s
	}
	api := serveEngine(engine)
	writes := &atomic.Int32{}
	client := contentkit.NewClient(api.URL+prefix, &http.Client{Transport: publisherTransport{key.Secret, writes}})

	created, err := client.PostContents(ctx, catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "A separately uploaded gallery"}}})
	if err != nil || len(created.Versions) != 1 {
		t.Fatalf("create draft: %+v %v", created, err)
	}
	ref := media.RefBody{Kind: "gallery", ID: created.ID, Version: created.Versions[0].ID}
	upload := func(ref media.RefBody, payload []byte, name string) media.CommitBody {
		t.Helper()
		sum := sha256.Sum256(payload)
		writes.Store(0)
		plan, err := client.PostMediaUploadPresign(ctx, media.PresignBody{Ref: ref, Path: "import/" + name, Type: "application/zip", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])})
		if err != nil || plan.Put == nil {
			t.Fatalf("presign: %+v %v", plan, err)
		}
		req, err := http.NewRequestWithContext(ctx, plan.Put.Method, plan.Put.URL, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range plan.Put.Headers {
			req.Header.Set(k, v)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("storage PUT: %d", response.StatusCode)
		}
		commit := media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob, SourceName: plan.SourceName}}}
		if _, err := client.PostMediaUploadCommit(ctx, commit); err != nil {
			t.Fatal(err)
		}
		if writes.Load() != 2 {
			t.Fatalf("upload used %d API writes, want presign+commit", writes.Load())
		}
		return commit
	}
	commit := upload(ref, uploadZIP(t), "pages.zip")
	pending, err := client.GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, contentkit.GetMediaVersionQuery{Status: true})
	if err != nil || pending.Status == nil || pending.Status.State != "processing" {
		t.Fatalf("pending status: %+v %v", pending, err)
	}
	if len(pending.Files) != 0 || pending.Expires != 0 {
		t.Fatalf("status minted capabilities: %+v", pending)
	}
	api.Close()
	if err = engine.Close(ctx); err != nil {
		t.Fatal(err)
	}
	engine, err = contentkit.New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	api = serveEngine(engine)
	client = contentkit.NewClient(api.URL+prefix, &http.Client{Transport: publisherTransport{key.Secret, writes}})
	regConfig, err := cfg.MediaKinds()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := media.NewRegistry(regConfig)
	if err != nil {
		t.Fatal(err)
	}
	workerInstance, err := worker.New(ctx, worker.Config{Pool: pool, Schema: schema + "_media_worker", ContentSchema: schema, Store: env.Store, Kinds: registry, HostSchema: schema + "_river", TempDir: t.TempDir(), Threads: 1, ImageWorkers: 1, VideoWorkers: 1, VideoEncoder: "cpu", VideoCodecs: []media.Codec{media.CodecH264}})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, workerStop := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- workerInstance.Run(workerCtx) }()
	t.Cleanup(func() {
		workerStop()
		if err := <-workerDone; err != nil {
			t.Error(err)
		}
	})
	waitStatus := func(ref media.RefBody, want string) *media.MediaStatus {
		t.Helper()
		for {
			read, err := client.GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, contentkit.GetMediaVersionQuery{Status: true})
			if err != nil {
				t.Fatal(err)
			}
			if read.Status != nil && read.Status.State == want {
				return read.Status
			}
			select {
			case <-ctx.Done():
				t.Fatalf("waiting for %s: %+v", want, read.Status)
				return nil
			case <-time.After(40 * time.Millisecond):
			}
		}
	}
	ready := waitStatus(ref, "ready")
	if ready.Archives.Completed != 1 || ready.Archives.Pages == nil || ready.Archives.Pages.Completed != 3 || ready.Archives.Pages.Total != 3 {
		t.Fatalf("ZIP progress: %+v", ready)
	}
	foundLow := false
	for _, preset := range ready.Presets {
		if preset.Name == "low" {
			foundLow = true
			if preset.Total != 3 || preset.Completed != 3 {
				t.Fatalf("rendition progress: %+v", preset)
			}
		}
	}
	if !foundLow {
		t.Fatalf("missing rendition progress: %+v", ready)
	}
	if _, err = client.PostMediaUploadCommit(ctx, commit); err != nil {
		t.Fatalf("replayed commit: %v", err)
	}
	var state string
	if err = pool.QueryRow(ctx, "SELECT state FROM "+pgx.Identifier{schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2", env.Tenant, created.ID).Scan(&state); err != nil || state != catalog.StateDraft {
		t.Fatalf("upload unexpectedly published: %s %v", state, err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1", env.Tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate gallery: %d %v", count, err)
	}
	title := "Metadata changed after processing"
	if _, err = client.PatchContent(ctx, created.ID, catalog.ContentPatch{OriginalTitle: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.PostContentPublish(ctx, created.ID, catalog.PublishInput{}); err != nil {
		t.Fatal(err)
	}
	// Normal media processing can finish exposure after explicit publication.
	waitStatus(ref, "ready")
	reply, err := client.GetContentVersionReader(ctx, created.ID, ref.Version)
	if err != nil {
		t.Fatal(err)
	}
	var plan versions.ReaderPlan
	if reply.Status != http.StatusOK || reply.Decode(&plan) != nil || plan.Pages == nil || *plan.Pages != 3 {
		t.Fatalf("reader pages after explicit publish: %+v", plan)
	}
	notices, err := client.GetContentVersionPageNotices(ctx, created.ID, ref.Version)
	if err != nil || len(notices.Notices) != 1 || notices.Notices[0].Kind != "reordered" {
		t.Fatalf("page notices: %+v %v", notices, err)
	}
	notice := notices.Notices[0].ID
	_, err = client.PostContentVersionPageNoticeRestore(ctx, created.ID, ref.Version, notice)
	var nothing *contentkit.Error
	if !errors.As(err, &nothing) || nothing.Status != http.StatusBadRequest {
		t.Fatalf("reorder restoration: %v", err)
	}
	if err = client.PostContentVersionPageNoticeDismiss(ctx, created.ID, ref.Version, notice); err != nil {
		t.Fatal(err)
	}
	bad, err := client.PostContents(ctx, catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Invalid archive stays a draft"}}})
	if err != nil || len(bad.Versions) != 1 {
		t.Fatalf("bad archive draft: %+v %v", bad, err)
	}
	badRef := media.RefBody{Kind: "gallery", ID: bad.ID, Version: bad.Versions[0].ID}
	upload(badRef, []byte("not a ZIP archive"), "invalid.zip")
	failed := waitStatus(badRef, "failed")
	if failed.FailureCount == 0 {
		t.Fatalf("missing processing failure: %+v", failed)
	}
	if err = pool.QueryRow(ctx, "SELECT state FROM "+pgx.Identifier{schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2", env.Tenant, bad.ID).Scan(&state); err != nil || state != catalog.StateDraft {
		t.Fatalf("failed upload published: %s %v", state, err)
	}
}

func uploadZIP(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, n := range []int{10, 2, 1} {
		f, err := zw.Create(fmt.Sprintf("page%d.png", n))
		if err != nil {
			t.Fatal(err)
		}
		img := image.NewRGBA(image.Rect(0, 0, 128, 192))
		for y := 0; y < 192; y++ {
			for x := 0; x < 128; x++ {
				img.SetRGBA(x, y, color.RGBA{R: uint8(x * n), G: uint8(y * n), B: uint8(n * 20), A: 255})
			}
		}
		if err = png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
