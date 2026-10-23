package migrations_test

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

func TestDirectInteractionCountsMigrationPreservesAggregate(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	schema := pgtest.EmptySchema(t, ctx, pool)
	q := pgx.Identifier{schema}.Sanitize()
	steps, err := migratekit.Load(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	boundary := slices.IndexFunc(steps, func(m migratekit.Migration) bool { return m.Name == "0035_direct_interaction_counts.up.sql" })
	if boundary < 0 {
		t.Fatal("direct counts migration missing")
	}
	migrator := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
	if err := migrator.ApplyMigrations(ctx, steps[:boundary]); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	id, v1, v2 := contentref.NewID(), contentref.NewID(), contentref.NewID()
	exec(`INSERT INTO `+q+`.content_items(tenant_id,id,kind) VALUES('t',$1,'gallery')`, id)
	exec(`INSERT INTO `+q+`.content_versions(tenant_id,id,content_id,languages) VALUES('t',$1,$3,'{en}'),('t',$2,$3,'{ja}')`, v1, v2, id)
	exec(`INSERT INTO `+q+`.content_facets(tenant_id,content_id,version_id,kind,languages,access_level,likes,favorites,rating) VALUES('t',$1,$2,'gallery','{en}','public',99,99,0.99),('t',$1,$3,'gallery','{ja}','public',88,88,0.88)`, id, v1, v2)
	exec(`INSERT INTO `+q+`.content_interaction_counts(tenant_id,content_kind,content_id,content_version_id,likes,dislikes,favorites) VALUES('t','gallery',$1,NULL,9,1,4),('t','gallery',$1,$2,100,0,100),('other','gallery',$1,NULL,200,0,200)`, id, v1)
	readAggregate := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY tenant_id,content_version_id)::text FROM `+q+`.content_interaction_counts c`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := readAggregate()
	if err := migrator.ApplyMigrations(ctx, steps); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='content_facets' AND column_name=ANY('{likes,dislikes,favorites,rating}'::text[])`, schema).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("copied columns=%d err=%v", columns, err)
	}
	if after := readAggregate(); after != before {
		t.Fatalf("migration changed authoritative totals: before=%s after=%s", before, after)
	}
	// A code rollback can restore the older projection from content totals,
	// without copying another tenant's or one version's counts into it.
	down, err := fs.ReadFile(migrations.Postgres, "0035_direct_interaction_counts.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+q+",public;"+string(down)); err != nil {
		t.Fatal(err)
	}
	var restored int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+q+`.content_facets WHERE likes=9 AND dislikes=1 AND favorites=4 AND rating BETWEEN 0.5 AND 0.9`).Scan(&restored); err != nil || restored != 2 {
		t.Fatalf("restored content projections=%d err=%v", restored, err)
	}
}
