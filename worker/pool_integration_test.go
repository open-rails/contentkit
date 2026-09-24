package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// poolSink reads through the pool on every delivery, as an outbox sink does.
type poolSink struct {
	t    *testing.T
	pool *pgxpool.Pool
	n    int
}

func (s *poolSink) check(ctx context.Context) error {
	s.n++
	return hostRead(ctx, s.t, s.pool)
}
func (s *poolSink) Upsert(ctx context.Context, _ search.PublishedDocument) error { return s.check(ctx) }
func (s *poolSink) Delete(ctx context.Context, _ search.DocumentKey, _ int64) error {
	return s.check(ctx)
}

// hostRead is a host callback's query: it needs a pooled connection, and the
// worker must hold none while it runs.
func hostRead(ctx context.Context, t *testing.T, pool *pgxpool.Pool) error {
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("worker holds %d connections across a host callback", n)
	}
	var one int
	return pool.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// A one-connection pool runs a whole tick (dirty rebuild and delete, sink and
// backfill) whose callbacks all query through that pool.
func TestIntegrationSyncOnSingleConnectionPool(t *testing.T) {
	ctx, base, schema := workerFixture(t)
	pool := singlePool(t, ctx, base)
	for _, id := range []string{"1", "2"} {
		if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{DocumentKey: search.DocumentKey{ContentRef: gallery(id), Language: "en"}, Deleted: id == "2"}}); err != nil {
			t.Fatal(err)
		}
	}
	opts := workerOptions(pool, schema, func(ctx context.Context, tenant, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		if err := hostRead(ctx, t, pool); err != nil {
			return nil, err
		}
		return titles(map[string]string{"1": "fresh", "3": "listed"})(ctx, tenant, kind, lang, refs)
	})
	opts.ListContent = func(ctx context.Context, _, _, _, _ string, _ int) ([]contentref.ContentRef, string, bool, error) {
		return []contentref.ContentRef{gallery("3")}, "3", true, hostRead(ctx, t, pool)
	}
	sink := &poolSink{t: t, pool: pool}
	opts.Sink = sink
	tick, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if err := SyncOnce(tick, opts); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if got := document(t, ctx, pool, schema); got != "fresh" {
		t.Fatal(got)
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "content_id='3'"); n != 1 {
		t.Fatal("backfilled document missing")
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 0 {
		t.Fatalf("dirty rows left: %d", n)
	}
	if sink.n != 3 {
		t.Fatalf("sink deliveries: %d", sink.n)
	}
}

// The hentai0 shape: a host holds one connection of a two-connection pool
// (its own run lock) while the tick's callbacks query the same pool.
func TestIntegrationSyncBesideHostHeldConnection(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	opts := workerOptions(pool, schema, func(ctx context.Context, tenant, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return nil, fmt.Errorf("host read: %w", err)
		}
		return titles(map[string]string{"1": "fresh"})(ctx, tenant, kind, lang, refs)
	})
	tick, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := SyncOnce(tick, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "fresh" {
		t.Fatal(got)
	}
}

func singlePool(t *testing.T, ctx context.Context, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
