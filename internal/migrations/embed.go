// Package migrations owns ContentKit's PostgreSQL and ClickHouse migrations.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/contentkit/internal/search"
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
	steps, err := migratekit.Load(Postgres, ".")
	if err != nil {
		return fmt.Errorf("contentkit: load PostgreSQL migrations: %w", err)
	}
	if err := requireCurrentHistory(ctx, db, "contentkit", schema, steps); err != nil {
		return err
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
	if err := requireCurrentHistory(ctx, db, "contentkit-accounts", schema, steps); err != nil {
		return err
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
	steps, err := migratekit.Load(Postgres, ".")
	if err != nil {
		return err
	}
	if err := requireCurrentHistory(ctx, db, "contentkit", schema, steps); err != nil {
		return err
	}
	return migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ValidateAllApplied(ctx, steps)
}

// ValidateAccounts is the read-only counterpart of ApplyAccounts.
func ValidateAccounts(ctx context.Context, db *sql.DB, schema string) error {
	if _, err := search.QuoteSchema(schema); err != nil {
		return err
	}
	steps, err := migratekit.Load(Accounts, ".")
	if err != nil {
		return err
	}
	if err := requireCurrentHistory(ctx, db, "contentkit-accounts", schema, steps); err != nil {
		return err
	}
	return migratekit.NewPostgres(db, "contentkit-accounts").WithSchema(schema).ValidateAllApplied(ctx, steps)
}

// requireCurrentHistory refuses a ledger written by a migration history this
// release no longer ships. migratekit only warns about it, and the database
// would otherwise start on an old schema. Such a database is recreated.
func requireCurrentHistory(ctx context.Context, db *sql.DB, app, schema string, steps []migratekit.Migration) error {
	records, err := migratekit.NewPostgres(db, app).WithSchema(schema).AppliedRecords(ctx)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "42P01" {
		return nil // a fresh database has no migration ledger yet
	}
	if err != nil {
		return err
	}
	current := map[string]migratekit.Migration{}
	for _, step := range steps {
		seq, err := migratekit.Sequence(step.Name)
		if err != nil {
			return err
		}
		current[strconv.FormatInt(seq, 10)] = step
	}
	for key, record := range records {
		step, ok := current[key]
		if ok && (record.Digest == migratekit.ContentDigest(step.Content) || record.SemanticDigest != "" && record.SemanticDigest == migratekit.SemanticContentDigest(step.Content)) {
			continue
		}
		return fmt.Errorf("contentkit: schema %q was migrated by an earlier %s migration history (migration %s, %s); recreate this database", schema, app, key, record.Filename)
	}
	return nil
}
