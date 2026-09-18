package content

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/migratekit"

	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
)

// MigratekitApp is the ledger label of the social lineage. It is an identity
// existing installations already carry, not vocabulary.
const MigratekitApp = "socialkit"

// Migrate applies the social lineage (migrations.Social) into the host schema
// under its ledger label, creating the schema when missing. Idempotent.
func Migrate(ctx context.Context, db *sql.DB, schema string) error {
	migs, err := migratekit.Load(migrations.Social, ".", migratekit.RequireParentLinks())
	if err != nil {
		return fmt.Errorf("content: load migrations: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return fmt.Errorf("content: ensure schema: %w", err)
	}
	if err := migratekit.NewPostgres(db, MigratekitApp).WithSchema(schema).ApplyMigrations(ctx, migs); err != nil {
		return fmt.Errorf("content: apply migrations: %w", err)
	}
	return nil
}

// tenantTables carry tenant_id.
var tenantTables = []string{"social_reactions", "social_comments", "social_poll_questions", "social_posts", "social_entity_counts", "social_favorites"}

// AssignTenant stamps tenant on every row the content-refs migration converted
// (tenant_id = ”): a host schema is single-tenant, so this is the one-time
// adoption step after the migration. Idempotent; returns the rows updated.
func AssignTenant(ctx context.Context, db search.Executor, schema, tenant string) (int64, error) {
	if tenant == "" {
		return 0, fmt.Errorf("content: tenant is required")
	}
	var total int64
	for _, table := range tenantTables {
		tag, err := db.Exec(ctx, `UPDATE `+pgx.Identifier{schema, table}.Sanitize()+` SET tenant_id = $1 WHERE tenant_id = ''`, tenant)
		if err != nil {
			return total, fmt.Errorf("content: assign tenant on %s: %w", table, err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}
