package migrations_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

// Dropping content moderation gives a live item with a held edit its approved
// text and paths back, returns a rejected never-published item to draft, and
// leaves other items as they are.
func TestContentModerationUpgrade(t *testing.T) {
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
	boundary := slices.IndexFunc(steps, func(step migratekit.Migration) bool { return step.Name == "0044_remove_content_moderation.up.sql" })
	if boundary < 0 {
		t.Fatal("content moderation migration is missing")
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps[:boundary]); err != nil {
		t.Fatal(err)
	}
	q := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
	if _, err := pool.Exec(ctx, `INSERT INTO `+q("content_items")+`(tenant_id,id,kind,state,published_at,moderation,moderation_reason,published_snapshot) VALUES
 ('test','held','gallery','published',now(),'held','awaiting review','{"en":{"title":"Harbor","slug":"harbor","description":"d","body":"b","sort_key":"","aliases":["Port"]}}'),
 ('test','other','gallery','published',now(),'approved',NULL,NULL),
 ('test','rejected','gallery','published',NULL,'rejected','no',NULL),
 ('test','pending','gallery','published',NULL,'held','awaiting review',NULL);
INSERT INTO `+q("content_text")+`(tenant_id,content_id,language,title,slug,aliases) VALUES
 ('test','held','en','Secret Name','secret-name','{}'), ('test','held','ja','Himitsu','himitsu','{}'),
 ('test','other','en','Other','other','{}');
INSERT INTO `+q("content_codes")+`(tenant_id,code,content_kind,content_id,slug,slugs) VALUES
 ('test','A1B2C3D4E','gallery','held','artist/secret-name','{"en":"artist/secret-name","ja":"artist/himitsu"}'),
 ('test','A1B2C3D4F','gallery','other','other','{"en":"other"}')`); err != nil {
		t.Fatal(err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps[:boundary+1]); err != nil {
		t.Fatal(err)
	}

	var texts, codes, states string
	if err := pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(content_id,language,title,slug,aliases) ORDER BY content_id,language)::text FROM `+q("content_text")).Scan(&texts); err != nil {
		t.Fatal(err)
	}
	if want := `[["held", "en", "Harbor", "harbor", ["Port"]], ["other", "en", "Other", "other", []]]`; texts != want {
		t.Fatalf("text %s, want %s", texts, want)
	}
	if err := pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(content_id,slug,slugs) ORDER BY content_id)::text FROM `+q("content_codes")).Scan(&codes); err != nil {
		t.Fatal(err)
	}
	if want := `[["held", "artist/harbor", {"en": "artist/harbor"}], ["other", "other", {"en": "other"}]]`; codes != want {
		t.Fatalf("codes %s, want %s", codes, want)
	}
	if err := pool.QueryRow(ctx, `SELECT string_agg(id||'='||state, ',' ORDER BY id) FROM `+q("content_items")).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != "held=published,other=published,pending=published,rejected=draft" {
		t.Fatalf("states %s", states)
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='content_items'
 AND (column_name LIKE 'moderat%' OR column_name='published_snapshot')`, schema).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("%d moderation columns remain: %v", columns, err)
	}
}
