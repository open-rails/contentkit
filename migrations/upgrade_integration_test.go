package migrations_test

import (
	"context"
	"io/fs"
	"slices"
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

// 0008 merges letter-case spellings of interaction keys (security audit
// 2026-10-01): each actor keeps its latest row, the merged keys are recounted
// and the tables refuse upper case afterwards.
func TestPostgresUpgradeMergesCaseSpellings(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	entries, err := fs.ReadDir(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	before := fstest.MapFS{}
	for _, e := range entries {
		if e.Name() < "0008" {
			data, err := fs.ReadFile(migrations.Postgres, e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: data}
		}
	}
	old, err := migratekit.Load(before, ".", migratekit.RequireParentLinks())
	if err != nil {
		t.Fatal(err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, old); err != nil {
		t.Fatal(err)
	}

	const (
		comment = "0192abcd-ef01-7abc-8def-abcdefabcdef"
		gallery = "0192abcd-ef01-7abc-8def-0123456789ab"
	)
	upper, mixed := strings.ToUpper(comment), "0192ABCD-ef01-7abc-8def-abcdefabcdef"
	seed := strings.NewReplacer("{s}", schema, "{c}", comment, "{C}", upper, "{m}", mixed, "{g}", gallery, "{G}", strings.ToUpper(gallery)).Replace(`
INSERT INTO {s}.content_comments (id, tenant_id, content_kind, content_id, user_id, body, likes, dislikes) VALUES
 ('{c}', 't', 'gallery', '{g}', 'author', 'hi', 3, 1),
 ('0192abcd-ef01-7abc-8def-000000000001', 't', 'gallery', '{G}', 'author', 'yo', 0, 0);
INSERT INTO {s}.content_reactions (tenant_id, content_kind, content_id, user_id, ip, value, revision) VALUES
 ('t', 'comment', '{c}', 'attacker', NULL, 1, 1), ('t', 'comment', '{C}', 'attacker', NULL, 1, 2),
 ('t', 'comment', '{m}', 'attacker', NULL, -1, 3), ('t', 'comment', '{C}', NULL, '203.0.113.9', 1, 4);
INSERT INTO {s}.content_favorites (tenant_id, content_kind, content_id, user_id, value, revision) VALUES
 ('t', 'gallery', '{g}', 'fan', 1, 5), ('t', 'gallery', '{G}', 'fan', 0, 6);
INSERT INTO {s}.content_interaction_counts (tenant_id, content_kind, content_id, likes, dislikes, favorites, comment_count) VALUES
 ('t', 'comment', '{c}', 1, 0, 0, 0), ('t', 'comment', '{C}', 2, 0, 0, 0), ('t', 'comment', '{m}', 0, 1, 0, 0),
 ('t', 'gallery', '{g}', 0, 0, 1, 1), ('t', 'gallery', '{G}', 0, 0, 0, 1)`)
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatal(err)
	}

	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	type row struct {
		kind, id, actor string
		value           int
	}
	var rows []row
	r, err := pool.Query(ctx, `SELECT content_kind, content_id, coalesce(user_id, ip), value FROM `+schema+`.content_reactions
 UNION ALL SELECT content_kind, content_id, user_id, value FROM `+schema+`.content_favorites ORDER BY 1, 3`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var x row
		if err := r.Scan(&x.kind, &x.id, &x.actor, &x.value); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, x)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{{"comment", comment, "203.0.113.9", 1}, {"comment", comment, "attacker", -1}, {"gallery", gallery, "fan", 0}}
	if !slices.Equal(rows, want) {
		t.Fatalf("merged rows = %+v, want %+v (each actor's latest)", rows, want)
	}
	var likes, dislikes int
	if err := pool.QueryRow(ctx, `SELECT likes, dislikes FROM `+schema+`.content_comments WHERE id = $1`, comment).Scan(&likes, &dislikes); err != nil || likes != 1 || dislikes != 1 {
		t.Fatalf("comment counters = %d/%d %v, want 1/1", likes, dislikes, err)
	}
	var counts string
	if err := pool.QueryRow(ctx, `SELECT string_agg(concat_ws(' ', content_kind, content_id, likes, dislikes, favorites, comment_count), ', ' ORDER BY content_kind)
 FROM `+schema+`.content_interaction_counts`).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	if want := "comment " + comment + " 1 1 0 0, gallery " + gallery + " 0 0 0 2"; counts != want {
		t.Fatalf("rollup = %q, want %q", counts, want)
	}
	var spelled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.content_comments WHERE content_id <> lower(content_id)`).Scan(&spelled); err != nil || spelled != 0 {
		t.Fatalf("upper-case comment targets left: %d %v", spelled, err)
	}
	for _, table := range []string{"content_reactions", "content_favorites", "content_interaction_counts", "content_comments"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2)`,
			schema+"."+table, table+"_content_id_ck").Scan(&ok); err != nil || !ok {
			t.Fatalf("%s lacks its lower-case check: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.content_reactions (tenant_id, content_kind, content_id, user_id, value, revision) VALUES ('t', 'comment', $1, 'u', 1, 9)`, upper); err == nil {
		t.Fatal("an upper-case content_id was stored")
	}
}
