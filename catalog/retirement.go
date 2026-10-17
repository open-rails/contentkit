package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetireOpenCounters removes the unused imported view-count columns. Call only
// after every app/importer sharing this catalog has upgraded; never on startup.
// Source images, content and reading resume/progress are unaffected.
func (s *Store) RetireOpenCounters(ctx context.Context, legacyWritersStopped bool) error {
	return retireCounterTables(ctx, s.pool, []string{s.t.items, s.t.facets}, legacyWritersStopped)
}

// RetireOpenCounters is the operator form; it opens no server/auth/billing runtime.
func RetireOpenCounters(ctx context.Context, pool *pgxpool.Pool, schema string, legacyWritersStopped bool) error {
	if pool == nil || schema == "" {
		return fmt.Errorf("catalog: pool and schema are required")
	}
	return retireCounterTables(ctx, pool, []string{pgx.Identifier{schema, "content_items"}.Sanitize(), pgx.Identifier{schema, "content_facets"}.Sanitize()}, legacyWritersStopped)
}
func retireCounterTables(ctx context.Context, pool *pgxpool.Pool, tables []string, legacyWritersStopped bool) error {
	if !legacyWritersStopped {
		return fmt.Errorf("catalog: stop legacy writers before retiring imported counters")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" DROP COLUMN IF EXISTS view_baseline"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
