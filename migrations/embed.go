// Package migrations embeds ContentKit's migratekit lineages: the keyword
// Postgres profile, the pre-ContentKit combined Postgres lineage that converges
// on it, and the ClickHouse signal plane.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed keyword/*.sql
var keywordFS embed.FS

// Postgres is the keyword profile: the only Postgres lineage for new
// installations. It requires pg_trgm and PGroonga and no vector extension.
// Apply it under its own migratekit app id, scoped to the host schema.
var Postgres fs.FS = mustSubFS(keywordFS, "keyword")

//go:embed legacy/*.sql
var legacyFS embed.FS

// LegacyPostgres is the pre-ContentKit combined lineage (searchkit app id).
// Installations created from it keep applying it: its final migration
// converges on the keyword profile schema and drops the embedding tables.
// Export those tables before applying it (docs/migration.md). Never use it
// for a new installation.
var LegacyPostgres fs.FS = mustSubFS(legacyFS, "legacy")

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

//go:embed clickhouse/signal/*.sql
var signalClickHouseFS embed.FS

// SignalClickHouse is the signal-plane ClickHouse lineage. Apply it with
// migratekit chmigrate to the dedicated signal database (see signal.CreateDatabase)
// under its own app identity; check compatibility at startup with
// signal.CheckSchema.
var SignalClickHouse fs.FS = mustSubFS(signalClickHouseFS, "clickhouse/signal")
