package migrations_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

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
	want := []string{"0001_baseline.up.sql", "0002_app_bindings.up.sql", "0003_prepare_version_covers.up.sql", "0004_finish_version_covers.up.sql", "0005_erasure_completion.up.sql", "0006_publishing_operations.up.sql", "0007_internal_entities.up.sql", "0008_access_history.up.sql", "0009_page_review.up.sql", "0010_name_prefix_index.up.sql", "0011_search_substrings.up.sql", "0012_management_names.up.sql", "0013_code_paths.up.sql", "0014_page_notices.up.sql", "0015_pages_added.up.sql", "0016_body_excerpt.up.sql", "0018_media_commit_notifications.up.sql"}
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
