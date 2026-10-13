package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

// The baseline installs into an empty schema once (applying again changes
// nothing), every key of its tables leads with tenant_id, and a content
// reference's version id is NULL for the work itself, never empty.
func TestPostgresBaseline(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	for range 2 {
		if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
			t.Fatal(err)
		}
	}
	var applied []string
	rows, err := pool.Query(ctx, `SELECT filename FROM public.migrations WHERE app = 'contentkit' AND schema = $1 ORDER BY sequence`, schema)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, f)
	}
	if err := rows.Err(); err != nil || len(applied) != 1 || applied[0] != "0001_baseline.up.sql" {
		t.Fatalf("ledger %v %v", applied, err)
	}

	keys, err := pool.Query(ctx, `SELECT c.relname || '.' || i.relname FROM pg_index x
 JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class c ON c.oid = x.indrelid
 JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = x.indkey[0]
 WHERE c.relnamespace = $1::regnamespace AND c.relname LIKE 'content\_%' AND x.indisunique AND a.attname <> 'tenant_id'`, schema)
	if err != nil {
		t.Fatal(err)
	}
	for keys.Next() {
		var k string
		if err := keys.Scan(&k); err != nil {
			t.Fatal(err)
		}
		t.Errorf("key %s does not lead with tenant_id", k)
	}
	if err := keys.Err(); err != nil {
		t.Fatal(err)
	}

	versions, err := pool.Query(ctx, `SELECT c.relname FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
 WHERE c.relnamespace = $1::regnamespace AND c.relkind = 'r' AND a.attname = 'content_version_id' AND NOT a.attisdropped
   AND (a.attnotnull OR NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'c'
     AND pg_get_constraintdef(k.oid) LIKE '%content_version_id%<> ''''::text%'))`, schema)
	if err != nil {
		t.Fatal(err)
	}
	for versions.Next() {
		var table string
		if err := versions.Scan(&table); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s.content_version_id is not NULL for the work itself", table)
	}
	if err := versions.Err(); err != nil {
		t.Fatal(err)
	}
}
