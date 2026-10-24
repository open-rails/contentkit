package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
)

func TestPreviousReleaseMigration(t *testing.T) {
	w := newWorld(t)
	schema := w.schema + "_previous"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, suffix := range []string{"_river", "_auth", ""} {
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema + suffix}.Sanitize()+" CASCADE")
			must(t, err)
		}
	})
	command := exec.CommandContext(t.Context(), "go", "run", "-p=1", ".")
	command.Dir = "previous-release"
	command.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2", "CONTENTKIT_PREVIOUS_SCHEMA="+schema)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		t.Fatalf("previous released initializer: %v\n%s", err, diagnostics.String())
	}
	var seeded struct {
		Version string          `json:"version"`
		Content catalog.Content `json:"content"`
	}
	must(t, json.Unmarshal(output, &seeded))
	if seeded.Version != "v0.125.0" || seeded.Content.ID == "" || len(seeded.Content.Versions) != 1 {
		t.Fatalf("fixture did not run pinned public release: %+v", seeded)
	}
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	auth, _ := authtest.New(t, authtest.WithConfig(func(cfg *authkit.Config) { cfg.Roles = roles }), authtest.WithDeps(func(deps *authkit.Deps) { deps.Postgres = w.pool }))
	cfg := ck.Config{Tenant: "previous-release", Database: ck.DatabaseConfig{Schema: schema, SkipMigrations: true}, Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"article": {Route: "articles", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}}}}
	deps := ck.Deps{Postgres: w.pool, Auth: auth.Authenticator()}
	t.Run("CK-J01-S02_prior_released_schema_requires_upgrade_and_preserves_identity", func(t *testing.T) {
		unprepared, err := ck.New(t.Context(), cfg, deps)
		if unprepared != nil || !errors.Is(err, ck.ErrMigrationsRequired) {
			t.Fatalf("old schema served before explicit migration: %v", err)
		}
		must(t, ck.MigratePostgres(t.Context(), w.pool, cfg))
		engine, err := ck.New(t.Context(), cfg, deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		server := mountEngine(t, engine)
		for name, client := range map[string]*ck.Client{"embedded_host": engine.Client(), "anonymous_HTTP": ck.NewClient(server.URL+"/content", nil)} {
			t.Run(name, func(t *testing.T) {
				got, err := client.GetContent(t.Context(), seeded.Content.ID, ck.GetContentQuery{})
				must(t, err)
				if got.ID != seeded.Content.ID || got.Code != seeded.Content.Code || got.Title != "Previous public release" || got.Body != "A durable previous-release manuscript" || len(got.Versions) != 1 || got.Versions[0].ID != seeded.Content.Versions[0].ID {
					t.Fatalf("migration changed public identity/read: %+v", got)
				}
			})
		}
		must(t, ck.MigratePostgres(t.Context(), w.pool, cfg))
		again, err := engine.Client().GetContent(t.Context(), seeded.Content.ID, ck.GetContentQuery{})
		must(t, err)
		if again.ID != seeded.Content.ID || again.Revision != seeded.Content.Revision {
			t.Fatal("repeated migration changed domain state")
		}
		must(t, w.pool.Ping(t.Context()))
	})
}
