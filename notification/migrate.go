package notification

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/notify/postgres"
	"strings"
)

// Migrate creates and upgrades a notification-only schema. Replicas serialize
// schema creation as well as the shared helper's versioned table migration.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if pool == nil || strings.TrimSpace(schema) != schema || schema == "" || len(schema) > 63 || strings.ContainsRune(schema, 0) {
		return fmt.Errorf("notification: valid pool and schema required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, schema+"/helpers/notify/create"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return postgres.Migrate(ctx, pool, schema)
}
