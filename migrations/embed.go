// Package migrations owns ContentKit's PostgreSQL and ClickHouse baselines.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/migratekit"
)

//go:embed postgres/*.sql clickhouse/*.sql
var files embed.FS

// Postgres installs all content, keyword and taxonomy tables in one host schema.
var Postgres fs.FS = mustSubFS("postgres")

// ClickHouse installs the signal-plane tables in the selected database.
var ClickHouse fs.FS = mustSubFS("clickhouse")

func mustSubFS(dir string) fs.FS {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// ApplyPostgres installs the complete baseline in schema and records it under
// the contentkit ledger identity. The schema may also hold the host's tables.
// Repeating the initializer preserves existing data and migration identity.
func ApplyPostgres(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil {
		return fmt.Errorf("contentkit: DB is required")
	}
	quoted, err := search.QuoteSchema(schema)
	if err != nil {
		return fmt.Errorf("contentkit: Schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
		return fmt.Errorf("contentkit: ensure schema: %w", err)
	}
	baseline, err := migratekit.Load(Postgres, ".", migratekit.RequireParentLinks())
	if err != nil {
		return fmt.Errorf("contentkit: load PostgreSQL baseline: %w", err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, baseline); err != nil {
		return fmt.Errorf("contentkit: apply PostgreSQL baseline: %w", err)
	}
	return nil
}
