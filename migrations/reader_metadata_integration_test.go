package migrations_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

func TestReaderMetadataMigrationRetiresOnlyDerivedDestinations(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial_destination", true: "ambiguous_source"}[ambiguous], func(t *testing.T) {
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
			boundary := slices.IndexFunc(steps, func(m migratekit.Migration) bool { return m.Name == "0028_reader_metadata_snapshots.up.sql" })
			if boundary < 0 {
				t.Fatal("reader migration missing")
			}
			migrator := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
			if err := migrator.ApplyMigrations(ctx, steps[:boundary]); err != nil {
				t.Fatal(err)
			}
			content, version, publication, destination := contentref.NewID(), contentref.NewID(), contentref.NewID(), contentref.NewID()
			source := "media-a/gallery/" + content + "/" + version + "/"
			legacy := "media-a/gallery.published/" + destination + "/"
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO `+q+`.content_items(tenant_id,id,kind) VALUES('catalog',$1,'gallery')`, content)
			exec(`INSERT INTO `+q+`.content_versions(tenant_id,id,content_id,languages) VALUES('catalog',$1,$2,'{en}')`, version, content)
			exec(`INSERT INTO `+q+`.content_version_publications(tenant_id,id,version_id,kind,state,source) VALUES('catalog',$1,$2,'pages','building','{"pages":[],"spans":[{"start":0,"end":10}]}')`, publication, version)
			exec(`INSERT INTO `+q+`.content_version_publication_items(tenant_id,id,publication_id,start_index,end_index) VALUES('catalog',$1,$2,0,10)`, destination, publication)

			insertSource := func(namespace, prefix string) {
				t.Helper()
				_, err := pool.Exec(ctx, `INSERT INTO `+q+`.content_media_commits
(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,'gallery',$3,$4,$5,'writer',$6,$7,'applied','{}')`, namespace, uuid.New(), content, version, prefix, make([]byte, 32), uuid.New())
				if err != nil {
					t.Fatal(err)
				}
			}
			insertSource("media-a", source)
			if ambiguous {
				insertSource("media-b", "media-b/gallery/"+content+"/"+version+"/")
			}
			// No destination journal exists: even this partially prepared span must
			// receive an ownership receipt and eventual folder cleanup.
			err = migrator.ApplyMigrations(ctx, steps[:boundary+1])
			if ambiguous {
				if err == nil || !strings.Contains(err.Error(), "unique source ownership") {
					t.Fatalf("ambiguous ownership accepted: %v", err)
				}
				var stillExists bool
				if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".content_version_publication_items").Scan(&stillExists); err != nil || !stillExists {
					t.Fatalf("failed migration discarded old ownership: %v %v", stillExists, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var prefix, sourceProof, state string
			var targets []string
			var infinite, hasSpans bool
			if err := pool.QueryRow(ctx, `SELECT c.folder_prefix,c.effects->'legacy_publication'->>'source',ARRAY(SELECT jsonb_array_elements_text(c.effects->'private')),d.due_at='infinity'::timestamptz
FROM `+q+`.content_media_commits c JOIN `+q+`.content_media_cleanup d ON d.tenant_id=c.tenant_id AND d.folder_prefix=c.folder_prefix
WHERE c.effects ? 'legacy_publication'`).Scan(&prefix, &sourceProof, &targets, &infinite); err != nil {
				t.Fatal(err)
			}
			if prefix != legacy || sourceProof != source || !infinite || len(targets) != 1 || targets[0] != legacy+"manifest.json" {
				t.Fatalf("unsafe destination receipt: %s %s %v %v", prefix, sourceProof, targets, infinite)
			}
			if err := pool.QueryRow(ctx, `SELECT state,source ? 'spans' FROM `+q+`.content_version_publications WHERE id=$1`, publication).Scan(&state, &hasSpans); err != nil || state != "current" || hasSpans {
				t.Fatalf("snapshot not preserved: %s spans=%v %v", state, hasSpans, err)
			}
		})
	}
}

func TestReaderMetadataMigrationRefusesInterruptedRetiredFolder(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	schema := pgtest.EmptySchema(t, ctx, pool)
	steps, err := migratekit.Load(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	boundary := slices.IndexFunc(steps, func(m migratekit.Migration) bool { return m.Name == "0028_reader_metadata_snapshots.up.sql" })
	if boundary < 0 {
		t.Fatal("reader migration missing")
	}
	m := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
	if err := m.ApplyMigrations(ctx, steps[:boundary]); err != nil {
		t.Fatal(err)
	}
	q := pgx.Identifier{schema}.Sanitize()
	id := contentref.NewID()
	operation := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO `+q+`.content_media_commits
(tenant_id,operation_id,content_kind,content_id,folder_prefix,actor_id,fingerprint,lease_id)
VALUES('media-a',$1,'gallery.published',$2,$3,'old-worker',$4,$5)`, operation, id, "media-a/gallery.published/"+id+"/", make([]byte, 32), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyMigrations(ctx, steps[:boundary+1]); err == nil || !strings.Contains(err.Error(), "every legacy publication operation settled") {
		t.Fatalf("interrupted retired folder accepted: %v", err)
	}
	var exists bool
	var state string
	var receipts int
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".content_version_publication_items").Scan(&exists); err != nil || !exists {
		t.Fatalf("old ownership schema changed: %v %v", exists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM `+q+`.content_media_commits WHERE operation_id=$1`, operation).Scan(&state); err != nil || state != "open" {
		t.Fatalf("old operation changed: %s %v", state, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+q+`.content_media_commits WHERE effects ? 'legacy_publication'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("partial retirement receipts created: %d %v", receipts, err)
	}
}
