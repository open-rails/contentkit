package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/publishing"
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

// Exercise the real HTTP, native app-bound API key, durable SQL operation,
// direct S3 upload, ZIP import and stock image worker as one workflow. Stop the
// engine after accepting the ZIP; a new engine must finish that same operation.
func TestPublishingArchiveSurvivesRestart(t *testing.T) {
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
		Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"artist", "series", "tag"}}}, GroupKinds: map[string]kinds.GroupKind{"artist": {Route: "artists"}, "series": {Route: "series"}, "tag": {Route: "tags"}}},
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
		rs, err := e.Routes(contentkit.Routes{Prefix: prefix, Groups: e.RouteGroups()})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		for _, r := range rs {
			path := r.Path
			if r.Method != "" {
				path = r.Method + " " + path
			}
			mux.Handle(path, r.Handler)
		}
		s := httptest.NewServer(mux)
		t.Cleanup(s.Close)
		return s
	}
	api := serveEngine(engine)
	writes := &atomic.Int32{}
	client := contentkit.NewClient(api.URL+prefix, &http.Client{Transport: publisherTransport{key.Secret, writes}})
	id := uuid.NewString()
	archive := publishingZIP(t)
	request := publishing.Request{Content: catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "A publisher integration gallery"}}}, Publish: publishing.WhenReady, CreateMissingEntities: true,
		Entities: []publishing.EntityReference{{Kind: "artist", Name: "Test Artist", Language: "en"}, {Kind: "series", Name: "Example Series", Language: "en"}, {Kind: "tag", Name: "Color", Language: "en"}}, Source: &publishing.Source{Provider: "test", ID: id}}
	interrupted, stop := context.WithCancel(ctx)
	first, err := client.PublishArchive(interrupted, contentkit.ArchivePublication{OperationID: id, File: bytes.NewReader(archive), Filename: "pages.zip", Request: request, PollInterval: 20 * time.Millisecond, OnOperation: func(op publishing.Operation) {
		if op.State == publishing.Processing {
			stop()
		}
	}})
	stop()
	if !errors.Is(err, context.Canceled) || first.ID != id || first.State != publishing.Processing {
		t.Fatalf("interrupted receipt=%+v err=%v", first, err)
	}
	if len(first.Entities) != 3 {
		t.Fatalf("missing entity resolution receipt: %+v", first.Entities)
	}
	for _, entity := range first.Entities {
		if entity.ID == "" || entity.Name == "" || !entity.Created {
			t.Fatalf("new entity not reported: %+v", entity)
		}
	}
	if writes.Load() != 2 {
		t.Fatalf("metadata submission and complete should be two API writes; got %d", writes.Load())
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
	workerInstance, err := worker.New(ctx, worker.Config{Pool: pool, Schema: schema + "_media_worker", ContentSchema: schema, Store: env.Store, Kinds: registry, HostSchema: schema + "_river", TempDir: t.TempDir(), Threads: 1, ImageWorkers: 1, VideoWorkers: 1})
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
	final, err := client.PublishArchive(ctx, contentkit.ArchivePublication{OperationID: id, File: bytes.NewReader(archive), Filename: "pages.zip", Request: request, PollInterval: 40 * time.Millisecond})
	if err != nil {
		t.Fatalf("resume receipt=%+v err=%v", final, err)
	}
	if final.ID != id || final.ContentID != first.ContentID || final.VersionID != first.VersionID || final.State != publishing.Published || final.Result == nil || final.Result.Pages != 3 || final.Result.Path == "" || final.Upload != nil {
		t.Fatalf("publication=%+v", final)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1", env.Tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate catalog items: %d %v", count, err)
	}
	var groups int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "content_memberships"}.Sanitize()+" WHERE tenant_id=$1 AND content_id=$2 AND state='active'", env.Tenant, final.ContentID).Scan(&groups); err != nil || groups != 3 {
		t.Fatalf("resolved groups: %d %v", groups, err)
	}
	changed := request
	changed.Content.Text = map[string]catalog.Text{"en": {Title: "Different input"}}
	_, err = client.PublishArchive(ctx, contentkit.ArchivePublication{OperationID: id, File: bytes.NewReader(archive), Filename: "pages.zip", Request: changed})
	var apiError *contentkit.Error
	if !errors.As(err, &apiError) || apiError.Code != "operation_conflict" {
		t.Fatalf("changed input should conflict: %v", err)
	}

	// A successful HTTP submission does not turn corrupt bytes into a
	// successful publication: the worker records a terminal, typed failure.
	invalid := request
	invalid.Source = nil
	failed, err := client.PublishArchive(ctx, contentkit.ArchivePublication{
		OperationID: uuid.NewString(), File: bytes.NewReader([]byte("not a ZIP archive")),
		Filename: "invalid.zip", Request: invalid, PollInterval: 40 * time.Millisecond,
	})
	var failure *publishing.OperationError
	if !errors.As(err, &failure) || failed.State != publishing.Failed || failure.Code != "invalid_archive" {
		t.Fatalf("invalid archive receipt=%+v err=%v", failed, err)
	}

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	scheduledRequest := request
	scheduledRequest.Source = nil
	scheduledRequest.PublishAt = &future
	scheduled, err := client.PublishArchive(ctx, contentkit.ArchivePublication{
		OperationID: uuid.NewString(), File: bytes.NewReader(archive), Filename: "scheduled.zip",
		Request: scheduledRequest, PollInterval: 40 * time.Millisecond,
	})
	if err != nil || scheduled.State != publishing.Scheduled || scheduled.PublishAt == nil || !scheduled.PublishAt.Equal(future) || scheduled.Result == nil || scheduled.Result.Pages != 3 {
		t.Fatalf("scheduled receipt=%+v err=%v", scheduled, err)
	}
	response, err := http.Get(api.URL + prefix + "/v1/contents/" + scheduled.ContentID)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("scheduled gallery became public early: HTTP %d", response.StatusCode)
	}

	draftRequest := request
	draftRequest.Source, draftRequest.Publish = nil, publishing.Draft
	draft, err := client.PublishArchive(ctx, contentkit.ArchivePublication{
		OperationID: uuid.NewString(), File: bytes.NewReader(archive), Filename: "draft.zip",
		Request: draftRequest, PollInterval: 40 * time.Millisecond,
	})
	if err != nil || draft.State != publishing.Ready || draft.Result == nil || draft.Result.Pages != 3 {
		t.Fatalf("ready draft receipt=%+v err=%v", draft, err)
	}
	response, err = http.Get(api.URL + prefix + "/v1/contents/" + draft.ContentID)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("draft gallery became public: HTTP %d", response.StatusCode)
	}
}

func publishingZIP(t *testing.T) []byte {
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
