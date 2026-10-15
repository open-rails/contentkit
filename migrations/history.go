package migrations

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/migratekit"
)

// This is the exact baseline shipped before covers became version-owned.
// Keep the bytes immutable: existing ledgers truthfully record this content.
// The numbered upgrade migrations bring it to the current schema instead of
// claiming that those installations ran the edited fresh-install baseline.
//
//go:embed compat/postgres_0001_before_version_covers.sql
var beforeVersionCovers string

const beforeVersionCoversDigest = "5f1614f38022a4c67426f22e55162ce043f16d3a917a34f9e1ca58903cac58dc"

func postgresChain(ctx context.Context, db *sql.DB, schema string) ([]migratekit.Migration, error) {
	steps, err := migratekit.Load(Postgres, ".")
	if err != nil {
		return nil, err
	}
	if migratekit.ContentDigest(beforeVersionCovers) != beforeVersionCoversDigest {
		return nil, errors.New("contentkit: immutable pre-version-cover baseline was edited")
	}
	records, err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).AppliedRecords(ctx)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "42P01" {
		return steps, nil // a fresh database has no migration ledger yet
	}
	if err != nil {
		return nil, err
	}
	old, exists := records["1"]
	if !exists {
		return steps, nil
	}
	if old.Filename != "" && old.Filename != steps[0].Name {
		return nil, fmt.Errorf("contentkit: migration 1 records unexpected file %q", old.Filename)
	}
	if old.Digest == beforeVersionCoversDigest || old.SemanticDigest != "" && old.SemanticDigest == migratekit.SemanticContentDigest(beforeVersionCovers) {
		steps[0].Content = beforeVersionCovers
		return steps, nil
	}
	if old.Digest != "" && old.Digest != migratekit.ContentDigest(steps[0].Content) &&
		(old.SemanticDigest == "" || old.SemanticDigest != migratekit.SemanticContentDigest(steps[0].Content)) {
		return nil, fmt.Errorf("contentkit: unrecognized baseline history %q; explicit migration is required before this release", old.Digest)
	}
	return steps, nil
}
