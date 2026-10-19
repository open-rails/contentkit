package search_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
)

// A group merge may supply its dirty documents in map order. The publisher
// locks them in key order; the writer must do the same to avoid a deadlock.
func TestMarkDirtyOrdersBatchLocks(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	mark := func(id string) search.DirtyMark {
		return search.DirtyMark{DocumentKey: search.DocumentKey{ContentRef: contentref.New("order", "tag", id), Language: "en"}}
	}
	first := mark("01920000-0000-7000-8000-000000000001")
	second := mark("01920000-0000-7000-8000-000000000002")
	if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{first, second}); err != nil {
		t.Fatal(err)
	}
	publisher, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Rollback(context.Background())
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := publisher.Exec(ctx, `SELECT 1 FROM `+pgx.Identifier{schema, "content_search_dirty"}.Sanitize()+` WHERE content_id=$1 FOR UPDATE`, first.ContentID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- search.MarkDirty(ctx, writer, schema, []search.DirtyMark{second, first}) }()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, writer.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("writer did not wait on the publisher: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// With reversed writer locks this waits for the second row while the
	// writer waits for our first row: a real PostgreSQL deadlock.
	if _, err := publisher.Exec(ctx, `SELECT 1 FROM `+pgx.Identifier{schema, "content_search_dirty"}.Sanitize()+` WHERE content_id=$1 FOR UPDATE`, second.ContentID); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
