package migrations

import (
	"embed"
	"io/fs"
)

// migratekit's LoadFromFS does not recurse into subdirectories, so we expose a
// sub-filesystem rooted at "postgres/".
//
//go:embed postgres/*.sql
var postgresFS embed.FS

// Postgres is the immutable combined profile for existing installations.
var Postgres fs.FS = mustSubFS(postgresFS, "postgres")

//go:embed keyword/*.sql
var keywordFS embed.FS

// KeywordPostgres is a separate fresh-install profile, requiring pg_trgm and
// PGroonga but no vector extension or semantic tables. Use its own migration
// ledger/group identity; never replace an existing Postgres migration ledger.
// Existing combined installations can use keyword runtime while keeping their
// original profile and all optional semantic data.
var KeywordPostgres fs.FS = mustSubFS(keywordFS, "keyword")

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
