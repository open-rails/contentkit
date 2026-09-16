package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/searchkit/runtime"
)

func workerFixture(t *testing.T) (context.Context, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("SEARCHKIT_TEST_PG_DSN")
	if dsn == "" {
		dsn = os.Getenv("SEARCHKIT_TEST_URL")
	}
	if dsn == "" {
		t.Skip("SEARCHKIT_TEST_URL or SEARCHKIT_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	schema := fmt.Sprintf("worker_test_%d", time.Now().UnixNano())
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s;
 CREATE FUNCTION %s.searchkit_regconfig_for_language(text) RETURNS regconfig LANGUAGE sql IMMUTABLE AS $$ SELECT 'simple'::regconfig $$;
 CREATE TABLE %s.search_dirty (entity_type text,entity_id text,language text,is_deleted boolean DEFAULT false,reason text DEFAULT 'test',created_at timestamptz DEFAULT now(),updated_at timestamptz DEFAULT now(),PRIMARY KEY(entity_type,entity_id,language));
 CREATE TABLE %s.search_documents (entity_type text,entity_id text,language text,raw_document text,document text,tsv tsvector,created_at timestamptz,updated_at timestamptz,PRIMARY KEY(entity_type,entity_id,language));
 CREATE TABLE %s.search_documents_backfill_state (entity_type text,language text,cursor text DEFAULT '',state text DEFAULT 'pending',last_error text,updated_at timestamptz DEFAULT now(),PRIMARY KEY(entity_type,language));`, schema, schema, schema, schema, schema))
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../migrations/postgres/0002_search_dirty_revision.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "SET search_path TO "+schema+";"+string(migration)+"; RESET search_path"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	return ctx, pool, schema
}

func workerRuntime(t *testing.T, pool *pgxpool.Pool, schema string, build runtime.BuildLexicalString) *runtime.Runtime {
	t.Helper()
	rt, err := runtime.New(runtime.Options{Pool: pool, Schema: schema, BuildLexicalString: build})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}
func workerOptions(pool *pgxpool.Pool, schema string) SearchkitOptions {
	return SearchkitOptions{Pool: pool, Schema: schema, SupportedLanguages: []string{"en"}, LexicalEntityTypes: []string{"gallery"}, ListEntityIDsPage: func(context.Context, string, string, string, int) ([]string, string, bool, error) {
		return nil, "", true, nil
	}}
}
func markDirty(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	_, err := pool.Exec(ctx, `INSERT INTO `+schema+`.search_dirty(entity_type,entity_id,language,updated_at) VALUES('gallery','1','en','2020-01-01') ON CONFLICT(entity_type,entity_id,language) DO UPDATE SET updated_at=EXCLUDED.updated_at`)
	return err
}
func document(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, "SELECT raw_document FROM "+schema+".search_documents WHERE entity_id='1'").Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIntegrationDirtyUpdateSurvivesConcurrentSync(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	rt := workerRuntime(t, pool, schema, func(context.Context, string, string, []string) (map[string]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return map[string]string{"1": "old"}, nil
		}
		return map[string]string{"1": "new"}, nil
	})
	opts := workerOptions(pool, schema)
	done := make(chan error, 1)
	go func() { done <- SyncOnce(ctx, rt, opts) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A competing tick must neither run the callback nor wait for this writer.
	if err := SyncOnce(ctx, rt, opts); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("overlapping writer entered callback")
	}
	// An equal-timestamp host update commits while the callback is in flight.
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+schema+".search_dirty").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("concurrent equal-timestamp update lost")
	}
	var stalePublished int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+schema+".search_documents").Scan(&stalePublished); err != nil {
		t.Fatal(err)
	}
	if stalePublished != 0 {
		t.Fatal("changed generation published stale document")
	}
	if err := SyncOnce(ctx, rt, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "new" {
		t.Fatalf("document=%q", got)
	}
}

func TestIntegrationSyncRollbackAndSingleConnection(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	rt := workerRuntime(t, pool, schema, func(context.Context, string, string, []string) (map[string]string, error) {
		return map[string]string{"1": "new"}, nil
	})
	opts := workerOptions(pool, schema)
	opts.ListEntityIDsPage = func(context.Context, string, string, string, int) ([]string, string, bool, error) {
		return nil, "", false, errors.New("backfill failed")
	}
	if err := SyncOnce(ctx, rt, opts); err == nil {
		t.Fatal("expected backfill failure")
	}
	for table, want := range map[string]int{"search_dirty": 1, "search_documents": 0, "search_documents_backfill_state": 0} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+schema+"."+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s=%d want %d", table, count, want)
		}
	}
	cfg := pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	opts.Pool = single
	if err := SyncOnce(ctx, rt, opts); err == nil {
		t.Fatal("one-connection pool must fail before callbacks")
	}
}

func TestIntegrationLostWriterCannotOverwrite(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	// A disconnected writer must not publish after a replacement commits.
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	rt := workerRuntime(t, pool, schema, func(context.Context, string, string, []string) (map[string]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return map[string]string{"1": "stale"}, nil
		}
		return map[string]string{"1": "fresh"}, nil
	})
	opts := workerOptions(pool, schema)
	opts.ListEntityIDsPage = func(context.Context, string, string, string, int) ([]string, string, bool, error) {
		return []string{"1"}, "1", true, nil
	}
	done := make(chan error, 1)
	go func() { done <- SyncOnce(ctx, rt, opts) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var terminated bool
	err := pool.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND granted AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid AND objsubid=1`, "searchkit:sync:"+schema).Scan(&terminated)
	if err != nil || !terminated {
		t.Fatalf("terminate: %v %v", terminated, err)
	}
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := SyncOnce(ctx, rt, opts); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("lost writer must fail")
	}
	if got := document(t, ctx, pool, schema); got != "fresh" {
		t.Fatalf("stale writer overwrote: %q", got)
	}
}

func TestIntegrationBackfillQueuesSameWriter(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	var calls atomic.Int32
	rt := workerRuntime(t, pool, schema, func(context.Context, string, string, []string) (map[string]string, error) {
		calls.Add(1)
		return map[string]string{"1": "fresh"}, nil
	})
	opts := workerOptions(pool, schema)
	opts.ListEntityIDsPage = func(context.Context, string, string, string, int) ([]string, string, bool, error) {
		return []string{"1"}, "1", true, nil
	}
	if err := SyncOnce(ctx, rt, opts); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("backfill bypassed dirty writer")
	}
	if err := SyncOnce(ctx, rt, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "fresh" {
		t.Fatal(got)
	}
}

func TestIntegrationDirtyRevisionNeverReusesGeneration(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	var previous int64
	for i := 0; i < 3; i++ {
		if i == 2 {
			if _, err := pool.Exec(ctx, "DELETE FROM "+schema+".search_dirty"); err != nil {
				t.Fatal(err)
			}
		}
		if err := markDirty(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
		var revision int64
		if err := pool.QueryRow(ctx, "SELECT revision FROM "+schema+".search_dirty").Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if revision <= previous {
			t.Fatalf("generation reused: %d after %d", revision, previous)
		}
		previous = revision
	}
}
