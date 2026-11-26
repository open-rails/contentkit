package workqueue

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/migrations"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/migratekit"
)

// Validate checks the media worker's River and video-run storage without
// creating schemas, applying migrations or rewriting queued jobs.
func Validate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if err := ValidSchema(schema); err != nil {
		return err
	}
	if err := riverhelpers.ValidateMigrations(ctx, pool, riverhelpers.MigrationOptions{Schema: schema}); err != nil {
		return err
	}
	scripts, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	steps, err := migratekit.Load(scripts, ".")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	if err := migrations.RequireCurrentHistory(ctx, db, ledgerApp, schema, steps); err != nil {
		return err
	}
	if err := migratekit.NewPostgres(db, ledgerApp).WithSchema(schema).ValidateAllApplied(ctx, steps); err != nil {
		return fmt.Errorf("media/workqueue: run Migrate for %s: %w", schema, err)
	}
	name := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, `SELECT id,tenant_id,ref,state FROM `+name+`.encode_run LIMIT 0; SELECT run_id,ordinal,state FROM `+name+`.encode_chunk LIMIT 0`); err != nil {
		return fmt.Errorf("media/workqueue: storage missing; run Migrate for %s: %w", schema, err)
	}
	return nil
}
