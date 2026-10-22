package migrations_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

func cleanupMigrationSQL(t *testing.T, accounts bool) string {
	t.Helper()
	files, name := migrations.Postgres, "0027_due_media_cleanup.up.sql"
	if accounts {
		files, name = migrations.Accounts, "0007_due_media_cleanup.up.sql"
	}
	data, err := fs.ReadFile(files, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func cleanupMigrationTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	ctx := t.Context()
	schema := pgtest.EmptySchema(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	// These are the journal columns the real cleanup migrations consume. The
	// other journal fields cannot affect shared-table creation or bootstrap.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+pgx.Identifier{schema}.Sanitize()+`,public;
CREATE TABLE content_media_commits (tenant_id text,folder_prefix text,content_kind text);
INSERT INTO content_media_commits VALUES
    ('catalog','same-prefix/','gallery'),('accounts','same-prefix/','avatar');`); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestCleanupMigrationsShareSchemaInEitherOrder(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	for _, accountsFirst := range []bool{false, true} {
		name := "catalog_then_accounts"
		if accountsFirst {
			name = "accounts_then_catalog"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			tx := cleanupMigrationTx(t, pool)
			first, second := cleanupMigrationSQL(t, accountsFirst), cleanupMigrationSQL(t, !accountsFirst)
			if _, err := tx.Exec(ctx, first); err != nil {
				t.Fatal(err)
			}
			// Another lineage must not reset a real cleanup deadline or erase its
			// compare-and-swap revision and retained deletion operation receipts.
			if _, err := tx.Exec(ctx, `UPDATE content_media_cleanup
SET due_at='2026-10-23 12:00:00+00',revision='01920000-0000-7000-8000-000000000001',
    deletion_operations=ARRAY['01920000-0000-7000-8000-000000000002'::uuid]
WHERE tenant_id='catalog';`); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := tx.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM content_media_cleanup c WHERE tenant_id='catalog'`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO content_media_commits VALUES ('new-account','new-prefix/','avatar')`); err != nil {
				t.Fatal(err)
			}
			for _, sql := range []string{second, first, second} {
				if _, err := tx.Exec(ctx, sql); err != nil {
					t.Fatal(err)
				}
				var after string
				var count, bootstrapped int
				if err := tx.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM content_media_cleanup c WHERE tenant_id='catalog'`).Scan(&after); err != nil || after != before {
					t.Fatalf("existing cleanup changed: before=%s after=%s error=%v", before, after, err)
				}
				if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE due_at='infinity') FROM content_media_cleanup`).Scan(&count, &bootstrapped); err != nil || count != 3 || bootstrapped != 2 {
					t.Fatalf("bootstrap lost tenant isolation or repeated rows: count=%d bootstrap=%d error=%v", count, bootstrapped, err)
				}
			}
		})
	}
}

func TestCleanupMigrationsRejectIncompatibleSharedRelation(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	for _, accountsFirst := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			sql  string
		}{
			{"wrong_type", `ALTER TABLE content_media_cleanup ALTER COLUMN revision DROP DEFAULT;
ALTER TABLE content_media_cleanup ALTER COLUMN revision TYPE text USING revision::text`},
			{"nullable_deadline", `ALTER TABLE content_media_cleanup ALTER COLUMN due_at DROP NOT NULL`},
			{"missing_revision_default", `ALTER TABLE content_media_cleanup ALTER COLUMN revision DROP DEFAULT`},
			{"wrong_primary_key", `ALTER TABLE content_media_cleanup DROP CONSTRAINT content_media_cleanup_pkey;
ALTER TABLE content_media_cleanup ADD PRIMARY KEY(folder_prefix,tenant_id)`},
			{"wrong_due_index", `DROP INDEX content_media_cleanup_due;
CREATE INDEX content_media_cleanup_due ON content_media_cleanup(due_at,tenant_id,content_kind,folder_prefix)`},
		} {
			name := "catalog_then_accounts/" + tc.name
			if accountsFirst {
				name = "accounts_then_catalog/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				tx := cleanupMigrationTx(t, pool)
				if _, err := tx.Exec(ctx, cleanupMigrationSQL(t, accountsFirst)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, tc.sql); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, cleanupMigrationSQL(t, !accountsFirst)); err == nil || !strings.Contains(err.Error(), "incompatible content_media_cleanup") {
					t.Fatalf("incompatible shared schema accepted or failed for another reason: %v", err)
				}
			})
		}
	}
}
