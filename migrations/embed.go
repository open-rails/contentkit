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

//go:embed postgres/*.sql clickhouse/*.sql accounts/*.sql
var files embed.FS

// Postgres installs ContentKit's tables in its own schema.
var Postgres fs.FS = mustSubFS("postgres")

// ClickHouse installs the signal-plane tables in the selected database.
var ClickHouse fs.FS = mustSubFS("clickhouse")

// Accounts records ownership claims in the effective shared account schema.
// It is a separate lineage because apps can use different catalog schemas.
var Accounts fs.FS = mustSubFS("accounts")

func mustSubFS(dir string) fs.FS {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// ApplyPostgres applies ContentKit's migrations in its schema under its
// ledger identity and migration lock (contentkit.New calls it).
func ApplyPostgres(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil {
		return fmt.Errorf("contentkit: DB is required")
	}
	if _, err := search.QuoteSchema(schema); err != nil {
		return fmt.Errorf("contentkit: Schema: %w", err)
	}
	// MigrateKit creates the selected schema under its migration lock.
	steps, err := postgresChain(ctx, db, schema)
	if err != nil {
		return fmt.Errorf("contentkit: load PostgreSQL migrations: %w", err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps); err != nil {
		return fmt.Errorf("contentkit: apply PostgreSQL migrations: %w", err)
	}
	return nil
}

// ApplyAccounts installs account ownership metadata without moving inbox rows
// or media objects. The identity claim itself joins the app binding transaction.
func ApplyAccounts(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil {
		return fmt.Errorf("contentkit: DB is required")
	}
	if _, err := search.QuoteSchema(schema); err != nil {
		return fmt.Errorf("contentkit: account schema: %w", err)
	}
	steps, err := migratekit.Load(Accounts, ".")
	if err != nil {
		return fmt.Errorf("contentkit: load account migrations: %w", err)
	}
	if err := migratekit.NewPostgres(db, "contentkit-accounts").WithSchema(schema).ApplyMigrations(ctx, steps); err != nil {
		return fmt.Errorf("contentkit: apply account migrations: %w", err)
	}
	return nil
}

// ValidatePostgres checks the catalog lineage without creating its schema or
// migration ledger. Runtime credentials only need read access to the ledger.
func ValidatePostgres(ctx context.Context, db *sql.DB, schema string) error {
	if _, err := search.QuoteSchema(schema); err != nil {
		return err
	}
	steps, err := postgresChain(ctx, db, schema)
	if err != nil {
		return err
	}
	return migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ValidateAllApplied(ctx, steps)
}

// ValidateAccounts is the read-only counterpart of ApplyAccounts.
func ValidateAccounts(ctx context.Context, db *sql.DB, schema string) error {
	if _, err := search.QuoteSchema(schema); err != nil {
		return err
	}
	return migratekit.ValidatePostgresMigrations(ctx, db, migratekit.MigrationSource{App: "contentkit-accounts", Schema: schema, FS: Accounts})
}
