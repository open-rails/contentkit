// Package pgtest provisions disposable keyword-profile schemas on the
// CONTENTKIT_TEST_URL Postgres for integration tests.
package pgtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureExtensions installs pg_trgm and PGroonga once in public under an
// advisory lock (CREATE EXTENSION IF NOT EXISTS is not concurrency-safe).
func EnsureExtensions(t testing.TB, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('contentkit:pgtest:extensions'));
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public; CREATE EXTENSION IF NOT EXISTS pgroonga SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// PGroonga < 4.1.0 lets VACUUM/ANALYZE in another session delete the
	// objects of an index created by a not-yet-committed transaction, so a
	// migration racing autovacuum leaves indexes failing "object isn't found"
	// (pgroonga GH-1013). The installed package's control file gives its version.
	var version string
	if err := pool.QueryRow(ctx, `SELECT default_version FROM pg_available_extensions WHERE name = 'pgroonga'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if !atLeast(version, 4, 1, 0) {
		t.Fatalf("PGroonga %s is too old: 4.1.0 or later is required (GH-1013); use groonga/pgroonga:4.1.0-debian-18 or newer", version)
	}
}

// atLeast reports whether a dotted version is at least major.minor.patch.
func atLeast(version string, want ...int) bool {
	parts := strings.SplitN(version, ".", len(want))
	for i, w := range want {
		if i >= len(parts) {
			return false
		}
		var n int
		if _, err := fmt.Sscanf(parts[i], "%d", &n); err != nil {
			return false
		}
		if n != w {
			return n > w
		}
	}
	return true
}

// MediaWorkerRole creates a login with exactly the grants production gives
// a host's media worker (doujins-gitops bind-roles: database CONNECT and
// CREATE, USAGE and CREATE on its schema, DML on its tables and sequences,
// USAGE on public and the host schemas) and returns its DSN. The role is
// dropped on cleanup.
func MediaWorkerRole(t testing.TB, ctx context.Context, admin *pgxpool.Pool, schema string, hostSchemas ...string) string {
	t.Helper()
	role := "ck_mw_" + fmt.Sprint(time.Now().UnixNano())
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	id := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	stmts := []string{
		"CREATE ROLE " + id(role) + " LOGIN PASSWORD 'worker'",
		"GRANT CONNECT, CREATE ON DATABASE " + id(db) + " TO " + id(role),
		"GRANT USAGE, CREATE ON SCHEMA " + id(schema) + " TO " + id(role),
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + id(schema) + " TO " + id(role),
		"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA " + id(schema) + " TO " + id(role),
		"GRANT USAGE ON SCHEMA public TO " + id(role),
	}
	for _, h := range hostSchemas {
		stmts = append(stmts, "GRANT USAGE ON SCHEMA "+id(h)+" TO "+id(role))
	}
	for _, q := range stmts {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP OWNED BY "+id(role))
		_, _ = admin.Exec(ctx, "DROP ROLE "+id(role))
	})
	cfg := admin.Config().ConnConfig
	return fmt.Sprintf("postgres://%s:worker@%s:%d/%s?sslmode=disable", role, cfg.Host, cfg.Port, db)
}
