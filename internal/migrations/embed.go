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

// Accounts installs ContentKit metadata in the shared tenant account schema.
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
// ledger identity and migration lock (contentkit.New calls it). Released
// migrations are immutable: a schema an earlier history built is refused.
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
	if err := RequireCurrentHistory(ctx, db, "contentkit", schema, steps); err != nil {
		return err
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).WithImmutableMigrations().ApplyMigrations(ctx, steps); err != nil {
		return fmt.Errorf("contentkit: apply PostgreSQL migrations: %w", err)
	}
	return nil
}

// ApplyAccounts installs account metadata without moving inbox rows or media objects.
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
	if err := RequireCurrentHistory(ctx, db, "contentkit-accounts", schema, steps); err != nil {
		return err
	}
	if err := migratekit.NewPostgres(db, "contentkit-accounts").WithSchema(schema).WithImmutableMigrations().ApplyMigrations(ctx, steps); err != nil {
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
	if err := RequireCurrentHistory(ctx, db, "contentkit", schema, steps); err != nil {
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
	if err := RequireCurrentHistory(ctx, db, "contentkit-accounts", schema, steps); err != nil {
		return err
	}
	return migratekit.NewPostgres(db, "contentkit-accounts").WithSchema(schema).ValidateAllApplied(ctx, steps)
}

// ErrEarlierHistory means a schema's migration ledger records a migration
// this release does not ship byte for byte: the schema was built before
// 0.141.0's baselines, or by an edited migration. It is rebuilt, never
// converted or migrated forward.
var ErrEarlierHistory = errors.New("contentkit: schema built by an earlier migration history")

// RequireCurrentHistory refuses a schema whose ledger for app records any
// migration other than the steps given, as given. A fresh database passes.
func RequireCurrentHistory(ctx context.Context, db *sql.DB, app, schema string, steps []migratekit.Migration) error {
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
		if step, ok := current[key]; ok && record.Digest == migratekit.ContentDigest(step.Content) {
			continue
		}
		return fmt.Errorf("%w: schema %q records %s migration %s (%s), which this release does not ship; rebuild it: drop the schema, DELETE FROM public.migrations WHERE app = '%s' AND schema = '%s', and migrate (docs/migration.md)",
			ErrEarlierHistory, schema, app, key, record.Filename, app, schema)
	}
	return nil
}
