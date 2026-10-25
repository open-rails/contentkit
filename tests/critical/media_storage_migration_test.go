package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
)

func TestLegacyMediaStorageMigration(t *testing.T) {
	t.Run("CK-J01-S02_real_legacy_version_media_copies_once_and_preserves_identity", func(t *testing.T) {
		w := newWorld(t)
		schema := w.schema + "_legacy"
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for _, suffix := range []string{"_media_worker", "_river", "_auth", ""} {
				_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema + suffix}.Sanitize()+" CASCADE")
				must(t, err)
			}
		})
		m := newMediaStorage(t, *w.apps[0].cfg.Scope)
		m.gateway.Close()
		gatewayConfig := m.gatewayConfig
		gatewayConfig.Routes = nil
		gatewayConfig.Hosts = map[string][]string{"127.0.0.1": {"previous-media"}}
		gatewayConfig.Buckets = map[string]string{"previous-media": m.bucket}
		handler, err := gateway.New(gatewayConfig)
		must(t, err)
		m.gateway = httptest.NewServer(handler)
		command := exec.CommandContext(t.Context(), "go", "run", "-p=1", ".")
		command.Dir = "previous-release/legacy-media"
		command.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2", "CONTENTKIT_LEGACY_MEDIA_SCHEMA="+schema, "CONTENTKIT_LEGACY_MEDIA_BUCKET="+m.bucket, "CONTENTKIT_LEGACY_MEDIA_URL="+m.gateway.URL, "CONTENTKIT_LEGACY_MEDIA_SIGNING="+m.signing)
		var diagnostics bytes.Buffer
		command.Stderr = &diagnostics
		output, err := command.Output()
		if err != nil {
			t.Fatalf("public legacy-media initializer: %v\n%s", err, diagnostics.String())
		}
		var old struct {
			Version string           `json:"version"`
			Content catalog.Content  `json:"content"`
			Ref     media.RefBody    `json:"ref"`
			Read    media.ReadResult `json:"read"`
			Payload string           `json:"payload"`
		}
		must(t, json.Unmarshal(output, &old))
		const payload = "Real ContentKit v0.112.1 nested version media.\n"
		if old.Version != "v0.112.1" || old.Content.ID == "" || old.Ref.ID != old.Content.ID || len(old.Content.Versions) != 1 || old.Content.Versions[0].ID != old.Ref.Version || len(old.Read.Files) != 1 || old.Payload != payload {
			t.Fatalf("initializer did not produce the pinned public legacy media: %+v", old)
		}
		readBytes := func(address string) {
			t.Helper()
			response, err := http.Get(address)
			must(t, err)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			must(t, err)
			if response.StatusCode != 200 || string(body) != payload {
				t.Fatalf("media bytes changed: HTTP%d %q", response.StatusCode, body)
			}
		}
		readBytes(old.Read.Files[0].URL)
		oldURL, err := url.Parse(old.Read.Files[0].URL)
		must(t, err)
		legacyPrefix := "/v1/previous-media/article/" + old.Ref.ID + "/" + old.Ref.Version + "/"
		if !strings.HasPrefix(oldURL.Path, legacyPrefix) {
			t.Fatalf("fixture already uses modern storage, not a real migration: %s", oldURL.Path)
		}
		flatManifest := "previous-media/article/" + old.Ref.Version + "/manifest.json"
		_, err = m.store.Head(t.Context(), flatManifest)
		if !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("target already exists before migration: %v", err)
		}
		roles := authkit.NewRoles()
		roles.Root.Declare(access.Names(access.Permissions()...)...)
		auth, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles }), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = w.pool }))
		cfg := ck.Config{Tenant: "previous-media", Database: ck.DatabaseConfig{Schema: schema, SkipMigrations: true}, Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"article": {Route: "articles", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}}}, Media: &ck.MediaConfig{Config: media.Config{BaseURL: m.gateway.URL, Kinds: []media.Kind{{Name: "article", KeepOriginals: true, ServeOriginals: true, Uploads: []media.Upload{{Path: "files/{name}", Types: []string{"text/plain"}, MaxBytes: 1024}}}}}, WorkerSchema: schema + "_media_worker", Delivery: ck.DeliveryConfig{Mode: string(media.DeliverURL), SigningKey: m.signing, TTL: time.Hour, Window: time.Second}}}
		deps := ck.Deps{Postgres: w.pool, Auth: auth.Authenticator(), Store: m.store}
		must(t, ck.MigratePostgres(t.Context(), w.pool, cfg))
		unprepared, err := ck.New(t.Context(), cfg, deps)
		if unprepared != nil || err == nil || !strings.Contains(err.Error(), "MigrateVersionStorage") {
			if unprepared != nil {
				_ = unprepared.Close(t.Context())
			}
			t.Fatalf("startup failed to refuse unmigrated real media: %v", err)
		}
		report, err := ck.MigrateVersionStorage(t.Context(), w.pool, cfg, m.store)
		must(t, err)
		if report.Versions != 1 || report.Copied != 1 || report.RetirementScheduled != 1 || report.Absent != 0 {
			t.Fatalf("nonempty migration did not copy its exact version: %+v", report)
		}
		must(t, w.pool.Ping(t.Context()))
		_, err = m.store.Head(t.Context(), flatManifest)
		must(t, err)
		engine, err := ck.New(t.Context(), cfg, deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		server := mountEngine(t, engine)
		client := ck.NewClient(server.URL+"/content", nil)
		got, err := client.GetContent(t.Context(), old.Content.ID, ck.GetContentQuery{})
		must(t, err)
		if got.ID != old.Content.ID || got.Code != old.Content.Code || got.Title != old.Content.Title || got.Body != old.Content.Body || len(got.Versions) != 1 || got.Versions[0].ID != old.Ref.Version {
			t.Fatalf("migration changed logical identity or authored content: %+v", got)
		}
		read, err := client.GetMediaVersion(t.Context(), old.Ref.Kind, old.Ref.ID, old.Ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		if len(read.Files) != 1 || read.Files[0].Path != "files/original.txt" {
			t.Fatalf("migrated public media missing: %+v", read)
		}
		newURL, err := url.Parse(read.Files[0].URL)
		must(t, err)
		if !strings.HasPrefix(newURL.Path, "/v1/previous-media/article/"+old.Ref.Version+"/") || strings.Contains(newURL.Path, old.Ref.ID) || path.Base(newURL.Path) != path.Base(oldURL.Path) {
			t.Fatalf("wrong migrated storage identity: %s -> %s", oldURL.Path, newURL.Path)
		}
		readBytes(read.Files[0].URL)
		readBytes(old.Read.Files[0].URL) // source is retained for its declared retirement grace
		again, err := ck.MigrateVersionStorage(t.Context(), w.pool, cfg, m.store)
		must(t, err)
		if again.Copied != 0 || again.Absent != 0 {
			t.Fatalf("repeated migration recopied or lost a live source: %+v", again)
		}
		replayed, err := client.GetMediaVersion(t.Context(), old.Ref.Kind, old.Ref.ID, old.Ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		if len(replayed.Files) != 1 || replayed.Files[0].Path != read.Files[0].Path {
			t.Fatalf("migration replay changed media: %+v", replayed)
		}
		replayedURL, err := url.Parse(replayed.Files[0].URL)
		must(t, err)
		if replayedURL.Path != newURL.Path {
			t.Fatal("migration replay replaced the immutable blob")
		}
		readBytes(replayed.Files[0].URL)
	})
}
