package migrations_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	notifypg "github.com/open-rails/helpers/notify/postgres"
	"github.com/open-rails/migratekit"
)

// A recorded historical baseline must keep its original identity, while an
// explicit upgrade makes its schema equal to a fresh installation. Covers are
// never discarded merely to make a schema or checksum agree.
func TestHistoricalBaselineUpgrade(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	fresh := pgtest.EmptySchema(t, ctx, pool)
	if err := migrations.ApplyPostgres(ctx, db, fresh); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile("compat/postgres_0001_before_version_covers.sql")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := migratekit.Load(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	chain = chain[:2]
	chain[0].Content = string(old)
	for _, hasCover := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_cover", true: "preserve_cover"}[hasCover], func(t *testing.T) {
			schema := pgtest.EmptySchema(t, ctx, pool)
			q := pgx.Identifier{schema}.Sanitize()
			if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, chain); err != nil {
				t.Fatal(err)
			}
			if err := notifypg.Migrate(ctx, pool, schema); err != nil {
				t.Fatal(err)
			}
			if hasCover {
				if _, err := pool.Exec(ctx, `INSERT INTO `+q+`.content_items(tenant_id,id,kind,cover) VALUES ('site','work','gallery','{"name":"original.webp"}');
INSERT INTO `+q+`.content_versions(tenant_id,id,content_id,code,languages) VALUES ('site','edition','work','ABCD','{en}')`); err != nil {
					t.Fatal(err)
				}
				err := migrations.ApplyPostgres(ctx, db, schema)
				if err == nil || !strings.Contains(err.Error(), "legacy covers require") {
					t.Fatalf("unprepared cover migration must refuse, got %v", err)
				}
				var name string
				if err := pool.QueryRow(ctx, `SELECT cover->>'name' FROM `+q+`.content_items WHERE id='work'`).Scan(&name); err != nil || name != "original.webp" {
					t.Fatalf("legacy metadata changed during refused migration: %q %v", name, err)
				}
				// The media migrator performs this metadata handoff only after its
				// corresponding object copies are verified.
				if _, err := pool.Exec(ctx, `UPDATE `+q+`.content_versions v SET cover=c.cover FROM `+q+`.content_items c WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id;
UPDATE `+q+`.content_items SET cover=NULL`); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
					t.Fatal(err)
				}
			}
			var digest string
			if err := pool.QueryRow(ctx, `SELECT content_sha256 FROM public.migrations WHERE app='contentkit' AND schema=$1 AND sequence=1`, schema).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			if digest != "5f1614f38022a4c67426f22e55162ce043f16d3a917a34f9e1ca58903cac58dc" {
				t.Fatalf("historical migration identity was rewritten: %s", digest)
			}
			if hasCover {
				var name string
				if err := pool.QueryRow(ctx, `SELECT cover->>'name' FROM `+q+`.content_versions WHERE id='edition'`).Scan(&name); err != nil || name != "original.webp" {
					t.Fatalf("version metadata was lost: %q %v", name, err)
				}
			}
			if diff, err := migratekit.SchemaDiff(ctx, db, schema, fresh); err != nil || len(diff) != 0 {
				t.Fatalf("upgraded schema differs from fresh: %v %v", diff, err)
			}
		})
	}
}

func TestHistoricalReaderMigrationUpgrade(t *testing.T) {
	for _, mode := range []string{"preserve_applied_0_114_1", "reject_unknown_history", "reject_empty_history"} {
		t.Run(mode, func(t *testing.T) {
			known := mode == "preserve_applied_0_114_1"
			ctx := t.Context()
			pool := pgtest.Pool(t, nil)
			pgtest.EnsureExtensions(t, ctx, pool)
			db := stdlib.OpenDBFromPool(pool)
			defer db.Close()
			schema := pgtest.EmptySchema(t, ctx, pool)
			old, err := os.ReadFile("compat/postgres_0028_before_bound_source.sql")
			if err != nil {
				t.Fatal(err)
			}
			if got := migratekit.ContentDigest(string(old)); got != "09a356813cd1761b0ec8a2f63e5f1b2b9638eff95fedfedc842e439921c2184a" {
				t.Fatalf("historical reader migration changed: %s", got)
			}
			chain, err := migratekit.Load(migrations.Postgres, ".")
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(chain, func(m migratekit.Migration) bool { return m.Name == "0028_reader_metadata_snapshots.up.sql" })
			if index < 0 {
				t.Fatal("reader migration missing")
			}
			chain[index].Content = string(old)
			if mode == "reject_unknown_history" {
				chain[index].Content += "\nSELECT 1;\n"
			}
			m := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
			if err := m.ApplyMigrations(ctx, chain[:index+1]); err != nil {
				t.Fatal(err)
			}
			if mode == "reject_empty_history" {
				// Simulate an incomplete imported ledger; no released reader
				// migration predates content and semantic digest recording.
				if _, err := pool.Exec(ctx, `UPDATE public.migrations SET content_sha256='',semantic_sha256='' WHERE app='contentkit' AND schema=$1 AND sequence=28`, schema); err != nil {
					t.Fatal(err)
				}
			}
			before, err := m.AppliedRecords(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				err := migrations.ApplyPostgres(ctx, db, schema)
				if !known {
					if err == nil || !strings.Contains(err.Error(), "unrecognized reader migration history") {
						t.Fatalf("unknown history accepted: %v", err)
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := migrations.ValidatePostgres(ctx, db, schema); err != nil {
					t.Fatalf("already-applied reader migration no longer validates: %v", err)
				}
			}
			after, err := m.AppliedRecords(ctx)
			if err != nil || before["28"] != after["28"] {
				t.Fatalf("historical reader migration record changed: %v", err)
			}
		})
	}
}
