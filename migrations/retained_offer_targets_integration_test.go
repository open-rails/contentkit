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

func TestRetainedOfferTargetsMigration(t *testing.T) {
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
	at := slices.IndexFunc(steps, func(m migratekit.Migration) bool { return m.Name == "0036_retained_offer_targets.up.sql" })
	if at < 0 {
		t.Fatal("missing retained targets migration")
	}
	runner := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
	if err = runner.ApplyMigrations(ctx, steps[:at]); err != nil {
		t.Fatal(err)
	}
	table := func(s string) string { return pgx.Identifier{schema, s}.Sanitize() }
	_, err = pool.Exec(ctx, `INSERT INTO `+table("content_items")+`(tenant_id,id,kind,state) VALUES ('test','live','post','published'),('test','gone','post','deleted');
 INSERT INTO `+table("content_versions")+`(tenant_id,id,content_id,languages,state) VALUES ('test','v','gone','{en}','deleted');
 INSERT INTO `+table("content_offers")+`(tenant_id,id,target_kind,target_id,terms) VALUES
 ('test','a','content','live','{"amount":1000000,"currency":"USD"}'),
 ('test','b','content','gone','{"amount":1000000,"currency":"USD"}'),
 ('test','c','version','v','{"amount":1000000,"currency":"USD"}');`)
	if err != nil {
		t.Fatal(err)
	}
	if err = runner.ApplyMigrations(ctx, steps[:at+1]); err != nil {
		t.Fatal(err)
	}
	var parent, state string
	var revision int64
	if err = pool.QueryRow(ctx, `SELECT content_id,state,revision FROM `+table("content_offers")+` WHERE id='c'`).Scan(&parent, &state, &revision); err != nil || parent != "gone" || state != "withdrawn" || revision != 2 {
		t.Fatalf("version: %q %q %d %v", parent, state, revision, err)
	}
	if err = pool.QueryRow(ctx, `SELECT state,revision FROM `+table("content_offers")+` WHERE id='a'`).Scan(&state, &revision); err != nil || state != "active" || revision != 1 {
		t.Fatalf("live: %q %d %v", state, revision, err)
	}
	if err = pool.QueryRow(ctx, `SELECT state,revision FROM `+table("content_offers")+` WHERE id='b'`).Scan(&state, &revision); err != nil || state != "withdrawn" || revision != 2 {
		t.Fatalf("deleted: %q %d %v", state, revision, err)
	}
	// Reapplying the migration must not increment the archival obligation again.
	if err = runner.ApplyMigrations(ctx, steps[:at+1]); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT revision FROM `+table("content_offers")+` WHERE id='c'`).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("replay revision: %d %v", revision, err)
	}
}
