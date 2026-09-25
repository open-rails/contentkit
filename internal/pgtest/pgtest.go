// Package pgtest provisions disposable keyword-profile schemas on the
// CONTENTKIT_TEST_URL Postgres for integration tests.
package pgtest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/migrations"
)

// Pool connects to CONTENTKIT_TEST_URL or skips; mutate may adjust the config.
func Pool(t testing.TB, mutate func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_TEST_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// EmptySchema creates a disposable schema (dropped on cleanup, its migratekit
// ledger rows included) for a test that applies a lineage itself.
func EmptySchema(t testing.TB, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	schema := fmt.Sprintf("ck_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
		_, _ = pool.Exec(ctx, "DELETE FROM public.migrations WHERE schema = $1", schema)
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	return schema
}

// EnsureExtensions installs pg_trgm and PGroonga once in public under an
// advisory lock (CREATE EXTENSION IF NOT EXISTS is not concurrency-safe).
func EnsureExtensions(t testing.TB, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('contentkit:pgtest:extensions'));
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public; CREATE EXTENSION IF NOT EXISTS pgroonga SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Schema creates a disposable schema, applies migrations.Postgres into it the
// way a host's migrate step does, and drops it on cleanup. The extensions are
// installed once in public: a schema drop must never take them away from a
// test running in another package, and CREATE EXTENSION IF NOT EXISTS is not
// concurrency-safe, so the first install is serialized.
func Schema(t testing.TB, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	schema := fmt.Sprintf("ck_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
	})
	EnsureExtensions(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+quoted+"; SET LOCAL search_path TO "+quoted+",public"); err != nil {
		t.Fatal(err)
	}
	files, err := fs.ReadDir(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		sql, err := fs.ReadFile(migrations.Postgres, f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f.Name(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return schema
}

// MediaWorkerRole creates a login with exactly the grants production gives
// a host's media worker (doujins-gitops bind-roles: database CONNECT and
// CREATE, USAGE and CREATE on its schema, DML on its tables and sequences,
// USAGE on public and the host schemas) and returns its DSN. The role is
// dropped on cleanup.
func MediaWorkerRole(t testing.TB, ctx context.Context, admin *pgxpool.Pool, schema string, hostSchemas ...string) string {
	t.Helper()
	role := "ck_mw_" + fmt.Sprint(time.Now().UnixNano())
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	id := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	stmts := []string{
		"CREATE ROLE " + id(role) + " LOGIN PASSWORD 'worker'",
		"GRANT CONNECT, CREATE ON DATABASE " + id(db) + " TO " + id(role),
		"GRANT USAGE, CREATE ON SCHEMA " + id(schema) + " TO " + id(role),
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + id(schema) + " TO " + id(role),
		"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA " + id(schema) + " TO " + id(role),
		"GRANT USAGE ON SCHEMA public TO " + id(role),
	}
	for _, h := range hostSchemas {
		stmts = append(stmts, "GRANT USAGE ON SCHEMA "+id(h)+" TO "+id(role))
	}
	for _, q := range stmts {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP OWNED BY "+id(role))
		_, _ = admin.Exec(ctx, "DROP ROLE "+id(role))
	})
	cfg := admin.Config().ConnConfig
	return fmt.Sprintf("postgres://%s:worker@%s:%d/%s?sslmode=disable", role, cfg.Host, cfg.Port, db)
}
