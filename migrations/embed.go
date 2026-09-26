// Package migrations owns ContentKit's PostgreSQL and ClickHouse migrations.
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

// ApplyPostgres applies ContentKit's migrations in schema under its ledger
// identity. The schema may also hold the host's tables.
func ApplyPostgres(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil {
		return fmt.Errorf("contentkit: DB is required")
	}
	if _, err := search.QuoteSchema(schema); err != nil {
		return fmt.Errorf("contentkit: Schema: %w", err)
	}
	// MigrateKit creates the selected schema under its migration lock.
	steps, err := migratekit.Load(Postgres, ".", migratekit.RequireParentLinks())
	if err != nil {
		return fmt.Errorf("contentkit: load PostgreSQL migrations: %w", err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps); err != nil {
		return fmt.Errorf("contentkit: apply PostgreSQL migrations: %w", err)
	}
	return nil
}
