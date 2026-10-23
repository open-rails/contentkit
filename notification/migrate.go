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
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+pgx.Identifier{schema, "notification_event_cursors"}.Sanitize()+` (
		namespace text NOT NULL,
		system text NOT NULL,
		origin_scope text NOT NULL,
		cursor text NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
		PRIMARY KEY (namespace,system,origin_scope)
	)`); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return postgres.Migrate(ctx, pool, schema)
}

// ValidateEventCursors checks the reader-owned state without creating tables at
// runtime. Hosts install it through their explicit migration command.
func ValidateEventCursors(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	rows, err := pool.Query(ctx, `SELECT namespace,system,origin_scope,cursor,updated_at FROM `+pgx.Identifier{schema, "notification_event_cursors"}.Sanitize()+` LIMIT 0`)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}
