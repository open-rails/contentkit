package migrations_test

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

func TestPostgresUpgradePreservesDirtyDocuments(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	baseline, err := fs.ReadFile(migrations.Postgres, "0001_baseline.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	old, err := migratekit.Load(fstest.MapFS{
		"0001_baseline.up.sql": {Data: baseline},
	}, ".", migratekit.RequireParentLinks())
	if err != nil {
		t.Fatal(err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.content_search_dirty
 (tenant_id,content_kind,content_id,content_version_id,language)
 VALUES ('doujins','gallery','01920000-0000-7000-8000-000000000001','','en')`); err != nil {
		t.Fatal(err)
	}

	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	var dirty int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.content_search_dirty`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 1 {
		t.Fatalf("dirty rows after upgrade = %d, want 1", dirty)
	}
	var invalidTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+`.content_search_invalid`).Scan(&invalidTable); err != nil {
		t.Fatal(err)
	}
	if !invalidTable {
		t.Fatal("invalid-document table missing after upgrade")
	}
}
