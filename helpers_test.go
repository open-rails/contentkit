package contentkit

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

const testTenant = "doujins"

func gallery(id string) ContentRef { return contentref.New(testTenant, "gallery", id) }

// doc builds one keyword document of the test tenant.
func doc(kind, id, lang, title string, aliases, keywords []string) KeywordDocument {
	return KeywordDocument{DocumentKey: DocumentKey{ContentRef: contentref.New(testTenant, kind, id), Language: lang}, Title: title, Aliases: aliases, Keywords: keywords}
}

// newTestPool is a lazily connecting pool for unit tests that never touch
// Postgres; port 1 refuses quickly if a query is attempted.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testPG connects to CONTENTKIT_TEST_URL or skips.
func testPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_TEST_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// keywordSchema creates a disposable schema and applies the keyword profile
// lineage to it, exactly as a host's migrate step does.
func keywordSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	schema := fmt.Sprintf("ck_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+quoted+"; SET LOCAL search_path TO "+quoted+",public"); err != nil {
		t.Fatal(err)
	}
	files, err := fs.ReadDir(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		sql, err := fs.ReadFile(migrations.Postgres, f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f.Name(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return schema
}

func upsertDocs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string, docs ...KeywordDocument) {
	t.Helper()
	if err := search.UpsertKeywordDocuments(ctx, pool, schema, docs); err != nil {
		t.Fatal(err)
	}
}

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// pgxpool connects lazily; these unit tests never touch Postgres.
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:9/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestHub(t *testing.T, ch signal.Conn, database string, mutate func(*EmbeddedConfig)) *EmbeddedHub {
	t.Helper()
	cfg := EmbeddedConfig{
		PG:       lazyPool(t),
		PGSchema: "hub",
		Tenant:   testTenant,
	}
	if ch != nil {
		cfg.CH = ch
		cfg.CHDatabase = database
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := NewEmbedded(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func ids(hits []SearchHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ContentID)
	}
	return out
}
