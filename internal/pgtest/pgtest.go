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

// Schema creates a disposable schema, applies migrations.Postgres into it the
// way a host's migrate step does, and drops it on cleanup. The extensions are
// installed once in public: a schema drop must never take them away from a
// test running in another package, and CREATE EXTENSION IF NOT EXISTS is not
// concurrency-safe, so the first install is serialized.
func Schema(t testing.TB, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	return SchemaWith(t, ctx, pool)
}

// SchemaWith is Schema followed by the given lineages, applied in order after
// the keyword profile (for example migrations.Taxonomy).
func SchemaWith(t testing.TB, ctx context.Context, pool *pgxpool.Pool, lineages ...fs.FS) string {
	t.Helper()
	schema := fmt.Sprintf("ck_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('contentkit:pgtest:extensions'));
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public; CREATE EXTENSION IF NOT EXISTS pgroonga SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+quoted+"; SET LOCAL search_path TO "+quoted+",public"); err != nil {
		t.Fatal(err)
	}
	for _, lineage := range append([]fs.FS{migrations.Postgres}, lineages...) {
		files, err := fs.ReadDir(lineage, ".")
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			sql, err := fs.ReadFile(lineage, f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				t.Fatalf("%s: %v", f.Name(), err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return schema
}
