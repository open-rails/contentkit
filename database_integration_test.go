package contentkit

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media/workqueue"
)

func migrationReadPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	read, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(read.Close)
	return read
}

func TestExplicitMigrateThenReadOnlyScopedStartup(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema, accounts := engineSchema(t, pool), engineSchema(t, pool)
	cfg := scopeConfig(schema, Scope{TenantID: NewTenantID(), AppID: NewAppID()}, "app")
	cfg.Accounts = &AccountsConfig{Schema: accounts}
	cfg.Database.SkipMigrations = true
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}}
	if e, err := New(ctx, cfg, deps); !errors.Is(err, ErrMigrationsRequired) {
		if e != nil {
			_ = e.Close(ctx)
		}
		t.Fatalf("unmigrated startup: %v", err)
	}
	for _, name := range []string{schema, accounts, schema + "_river"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, name).Scan(&exists); err != nil || exists {
			t.Fatalf("startup created %s: %t %v", name, exists, err)
		}
	}
	// No auth, billing or object-store dependency is supplied to this API.
	if err := MigratePostgres(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	ledger := func() string {
		t.Helper()
		var data string
		if err := pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.app,m.database,m.schema,m.sequence),'[]'::jsonb)::text FROM public.migrations m WHERE m.schema=ANY($1)`, []string{schema, accounts}).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := ledger()
	deps.Postgres = migrationReadPool(t, pool)
	e, err := New(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("read-only startup after explicit migrate: %v", err)
	}
	_ = e.Close(context.Background())
	if after := ledger(); after != before {
		t.Fatal("read-only startup changed migration ledger")
	}
	if _, err := pool.Exec(ctx, `UPDATE public.migrations SET status='running' WHERE app='contentkit-accounts' AND schema=$1 AND sequence=1`, accounts); err != nil {
		t.Fatal(err)
	}
	if e, err := New(ctx, cfg, deps); !errors.Is(err, ErrMigrationsRequired) {
		if e != nil {
			_ = e.Close(ctx)
		}
		t.Fatalf("pending account migration accepted: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM public.migrations WHERE app='contentkit-accounts' AND schema=$1 AND sequence=1`, accounts).Scan(&status); err != nil || status != "running" {
		t.Fatalf("runtime changed pending ledger: %q %v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE public.migrations SET status='applied' WHERE app='contentkit-accounts' AND schema=$1 AND sequence=1`, accounts); err != nil {
		t.Fatal(err)
	}
	// Missing binding is not created by runtime startup, even after schema migration.
	if _, err := pool.Exec(ctx, `DELETE FROM `+pgx.Identifier{schema, "content_app_bindings"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if e, err := New(ctx, cfg, deps); !errors.Is(err, ErrMigrationsRequired) {
		if e != nil {
			_ = e.Close(ctx)
		}
		t.Fatalf("missing binding accepted or recreated: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "content_app_bindings"}.Sanitize()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("runtime wrote binding: %d %v", rows, err)
	}
}

func TestMigrationAPIIncludesWorkerWithoutRuntimeDependencies(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := Config{Tenant: "legacy", Database: DatabaseConfig{Schema: schema, SkipMigrations: true}, Media: &MediaConfig{}}
	if err := MigratePostgres(t.Context(), pool, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Media.WorkerSchema != "" {
		t.Fatal("migration mutated caller config")
	}
	if err := workqueue.Validate(t.Context(), migrationReadPool(t, pool), schema+"_media_worker"); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitMigrationCanSkipQueueSchemas(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := Config{Tenant: "legacy", Database: DatabaseConfig{Schema: schema}, Media: &MediaConfig{}}
	if err := MigratePostgres(t.Context(), pool, cfg, MigrationOptions{SkipQueues: true}); err != nil {
		t.Fatal(err)
	}
	var catalog, river, worker bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL,to_regnamespace($2) IS NOT NULL,to_regnamespace($3) IS NOT NULL`, schema+".content_items", schema+"_river", schema+"_media_worker").Scan(&catalog, &river, &worker); err != nil {
		t.Fatal(err)
	}
	if !catalog || river || worker {
		t.Fatalf("skip queues created wrong schemas: catalog=%t river=%t worker=%t", catalog, river, worker)
	}
	if err := MigratePostgres(t.Context(), pool, cfg); err != nil {
		t.Fatal(err)
	}
	if err := workqueue.Validate(t.Context(), pool, schema+"_media_worker"); err != nil {
		t.Fatal(err)
	}
}
