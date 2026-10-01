package migrations_test

import (
	"context"
	"io/fs"
	"strings"
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
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.content_posts(id,tenant_id,author_id,title,body)
 VALUES ('01920000-0000-7000-8000-000000000001','doujins','author','title','body');
 INSERT INTO `+schema+`.content_poll_questions(id,tenant_id,question)
 VALUES ('01920000-0000-7000-8000-000000000002','doujins','question');
 INSERT INTO `+schema+`.content_poll_options(question_id,label)
 VALUES ('01920000-0000-7000-8000-000000000002','option')`); err != nil {
		t.Fatal(err)
	}
	for _, image := range []struct{ table, column string }{
		{"content_posts", "cover_url"}, {"content_poll_questions", "image_url"}, {"content_poll_options", "image_url"},
	} {
		t.Run(image.table, func(t *testing.T) {
			table := schema + "." + image.table
			legacyURL := "https://legacy.test/image.webp"
			if _, err := pool.Exec(ctx, `UPDATE `+table+` SET `+image.column+`=$1`, legacyURL); err != nil {
				t.Fatal(err)
			}
			if err := migrations.ApplyPostgres(ctx, db, schema); err == nil || !strings.Contains(err.Error(), "legacy image URLs") {
				t.Fatalf("populated image upgrade: %v", err)
			}
			var kept string
			if err := pool.QueryRow(ctx, `SELECT `+image.column+` FROM `+table).Scan(&kept); err != nil || kept != legacyURL {
				t.Fatalf("legacy URL changed: %q, err=%v", kept, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE `+table+` SET `+image.column+`=NULL`); err != nil {
				t.Fatal(err)
			}
		})
		if t.Failed() {
			return
		}
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
