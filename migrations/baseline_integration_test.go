package migrations_test

import (
	"context"
	"io/fs"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

func TestManagementOrderIndexUpgrade(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	steps, err := migratekit.Load(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	boundary := slices.IndexFunc(steps, func(step migratekit.Migration) bool {
		return step.Name == "0022_management_order_index.up.sql"
	})
	if boundary < 0 {
		t.Fatal("management order migration is missing")
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps[:boundary]); err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "content_items"}.Sanitize()
	if _, err := pool.Exec(ctx, `INSERT INTO `+table+`(tenant_id,id,kind,state,created_at)
 VALUES ('test','draft','gallery','draft','2026-10-01'),
        ('test','live','gallery','published','2026-10-01'),
        ('other','deleted','video','deleted','2026-09-30')`); err != nil {
		t.Fatal(err)
	}
	snapshot := `SELECT jsonb_agg(to_jsonb(c) ORDER BY tenant_id,id)::text FROM ` + table + ` c`
	var before string
	if err := pool.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
			t.Fatal(err)
		}
	}

	// Exercise rollback and reapplication without changing the migration ledger.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+pgx.Identifier{schema}.Sanitize()+`,public`); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"", "down", "up"} {
		if direction != "" {
			sql, err := fs.ReadFile(migrations.Postgres, "0022_management_order_index."+direction+".sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				t.Fatal(err)
			}
		}
		var after string
		if err := tx.QueryRow(ctx, snapshot).Scan(&after); err != nil || after != before {
			t.Fatalf("%s changed existing items: before=%s after=%s error=%v", direction, before, after, err)
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE i.indrelid=$1::regclass AND c.relname='content_items_management_order_idx'
   AND i.indisvalid AND i.indisready
   AND pg_get_indexdef(i.indexrelid) = format('CREATE INDEX content_items_management_order_idx ON %I.content_items USING btree (tenant_id, kind, created_at DESC, id DESC)', $2::text))`, table, schema).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if valid != (direction != "down") {
			t.Fatalf("%s: management order index valid=%t", direction, valid)
		}
		if direction == "down" {
			var absent bool
			if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, pgx.Identifier{schema, "content_items_management_order_idx"}.Sanitize()).Scan(&absent); err != nil || !absent {
				t.Fatalf("rollback retained management index: %v", err)
			}
		}
	}
}

func TestPublishingReconcileOrderUpgrade(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	steps, err := migratekit.Load(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	boundary := slices.IndexFunc(steps, func(step migratekit.Migration) bool {
		return step.Name == "0021_publishing_reconcile_order.up.sql"
	})
	if boundary < 0 {
		t.Fatal("publishing reconcile migration is missing")
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps[:boundary]); err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "content_publishing_operations"}.Sanitize()
	if _, err := pool.Exec(ctx, `INSERT INTO `+table+`(tenant_id,id,owner_key,request_hash,request,content_id,version_id,content_revision,version_revision,commit_id,state,publish_at,updated_at)
 VALUES('test','01920000-0000-7000-8000-000000000001','owner',repeat('a',64),'{}','content','version',1,1,'01920000-0000-7000-8000-000000000002','scheduled','2026-10-01','2026-09-30')`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(op)::text FROM `+table+` op`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
			t.Fatal(err)
		}
	}
	var after string
	var queued time.Time
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(op)-'reconcile_at')::text,reconcile_at FROM `+table+` op`).Scan(&after, &queued); err != nil || after != before || queued.IsZero() {
		t.Fatalf("upgrade changed the receipt or omitted scheduling: before=%s after=%s queued=%s error=%v", before, after, queued, err)
	}
	var indexes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname IN ('content_publishing_reconcile_idx','content_publishing_scheduled_idx')`, schema).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("publishing indexes: %d %v", indexes, err)
	}
	// Exercise the actual rollback SQL without changing the migration ledger.
	down, err := fs.ReadFile(migrations.Postgres, "0021_publishing_reconcile_order.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+pgx.Identifier{schema}.Sanitize()+`,public`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT to_jsonb(op)::text FROM `+table+` op`).Scan(&after); err != nil || after != before {
		t.Fatalf("rollback changed the receipt: after=%s error=%v", after, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='content_publishing_pending_idx'`, schema).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("rollback did not restore pending index: %d %v", indexes, err)
	}
}

func TestNamePrefixIndexUpgrade(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	steps, err := migratekit.Load(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	boundary := -1
	for i, step := range steps {
		if step.Name == "0010_name_prefix_index.up.sql" {
			boundary = i
			break
		}
	}
	if boundary < 0 {
		t.Fatal("name-prefix migration is missing")
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps[:boundary]); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, `INSERT INTO `+quoted+`.content_search_documents(tenant_id,content_kind,content_id,language,title,raw_document)
 VALUES('doujins','gallery','01920000-0000-7000-8000-000000000001','en','Business Question','Business Question Inazuma')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
			t.Fatal(err)
		}
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM `+quoted+`.content_search_documents`).Scan(&title); err != nil || title != "Business Question" {
		t.Fatalf("upgrade changed existing documents: %q %v", title, err)
	}
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=$1 AND indexname='content_search_documents_name_prefix')`, schema).Scan(&present); err != nil || !present {
		t.Fatalf("name prefix index missing after upgrade: %v", err)
	}
	var identity, searched, empty, voiced, unvoiced string
	if err := pool.QueryRow(ctx, `SELECT `+quoted+`.contentkit_keyword_normalize('The Café'),
 `+quoted+`.contentkit_search_normalize('The Café'), `+quoted+`.contentkit_search_normalize('the a an'),
 `+quoted+`.contentkit_search_normalize('ガンダム'), `+quoted+`.contentkit_search_normalize('カンタム')`).Scan(&identity, &searched, &empty, &voiced, &unvoiced); err != nil {
		t.Fatal(err)
	}
	if identity != "the cafe" || searched != "cafe" || empty != "" || voiced != "ガンダム" || unvoiced != "カンタム" {
		t.Fatalf("search normalization changed identity/native scripts: identity=%q search=%q filler=%q voiced=%q unvoiced=%q", identity, searched, empty, voiced, unvoiced)
	}
	var retained int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname=ANY($2::text[])`, schema, []string{
		"content_search_documents_keyword_exact", "content_search_documents_keyword_fuzzy", "content_search_documents_keyword_native", "content_search_documents_name_prefix",
		"content_search_documents_search_exact", "content_search_documents_search_fuzzy", "content_search_documents_search_native", "content_search_documents_search_substring",
		"content_search_documents_names_prefix", "content_search_documents_names_substring",
	}).Scan(&retained); err != nil || retained != 10 {
		t.Fatalf("rolling upgrade must retain old and new search indexes: %d %v", retained, err)
	}
}

// The catalog lineage installs into an empty schema once (applying again
// changes nothing), content keys lead with tenant_id, and a content reference's
// version id is NULL for the work itself, never empty. The ownership registry
// has separate unique app and catalog keys rather than content references.
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
	want := []string{"0001_baseline.up.sql", "0002_app_bindings.up.sql", "0003_prepare_version_covers.up.sql", "0004_finish_version_covers.up.sql", "0005_erasure_completion.up.sql", "0006_publishing_operations.up.sql", "0007_internal_entities.up.sql", "0008_access_history.up.sql", "0009_page_review.up.sql", "0010_name_prefix_index.up.sql", "0011_search_substrings.up.sql", "0012_management_names.up.sql", "0013_code_paths.up.sql", "0014_page_notices.up.sql", "0015_pages_added.up.sql", "0016_body_excerpt.up.sql", "0017_reader_chapter_starts.up.sql", "0018_media_commit_notifications.up.sql", "0019_offer_targets.up.sql", "0020_listing_order_index.up.sql", "0021_publishing_reconcile_order.up.sql", "0022_management_order_index.up.sql", "0023_playable_versions.up.sql"}
	if err := rows.Err(); err != nil || !slices.Equal(applied, want) {
		t.Fatalf("ledger %v %v", applied, err)
	}

	keys, err := pool.Query(ctx, `SELECT c.relname || '.' || i.relname FROM pg_index x
 JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class c ON c.oid = x.indrelid
 JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = x.indkey[0]
 WHERE c.relnamespace = $1::regnamespace AND c.relname LIKE 'content\_%' AND c.relname <> 'content_app_bindings'
   AND x.indisunique AND a.attname <> 'tenant_id'`, schema)
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
