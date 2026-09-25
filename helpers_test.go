package contentkit

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

const testTenant = "doujins"

// cid is a deterministic canonical UUIDv7 content id; cid(n) sorts in n order.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

// tax is a deterministic canonical UUIDv7 taxonomy id.
func tax(n int) string { return fmt.Sprintf("01930000-0000-7000-8000-%012d", n) }

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
func testPG(t *testing.T) *pgxpool.Pool { return pgtest.Pool(t, nil) }

// keywordSchema creates a disposable schema with the keyword profile applied.
func keywordSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	return pgtest.Schema(t, ctx, pool)
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
