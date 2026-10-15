package workqueue

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/migratekit"
)

// Validate checks the media worker's River and video-run storage without
// creating schemas, applying migrations or rewriting queued jobs.
func Validate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if err := ValidSchema(schema); err != nil {
		return err
	}
	if err := riverhelpers.ValidateMigrations(ctx, pool, schema); err != nil {
		return err
	}
	scripts, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	if err := migratekit.ValidatePostgresMigrations(ctx, db, migratekit.MigrationSource{App: "contentkit-media-worker", Schema: schema, FS: scripts}); err != nil {
		return fmt.Errorf("media/workqueue: run Migrate for %s: %w", schema, err)
	}
	name := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, `SELECT id,tenant_id,ref,state FROM `+name+`.encode_run LIMIT 0; SELECT run_id,ordinal,state FROM `+name+`.encode_chunk LIMIT 0`); err != nil {
		return fmt.Errorf("media/workqueue: storage missing; run Migrate for %s: %w", schema, err)
	}
	return nil
}
