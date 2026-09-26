package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
)

const tenant = "doujins"

func workerFixture(t *testing.T) (context.Context, *pgxpool.Pool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool := pgtest.Pool(t, func(cfg *pgxpool.Config) { cfg.MaxConns = 2 })
	return ctx, pool, pgtest.Schema(t, ctx, pool)
}

// cid is a deterministic canonical UUIDv7 content id; cid(n) sorts in n order.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func gallery(id string) contentref.ContentRef { return contentref.New(tenant, "gallery", id) }

// titles builds documents titled by the map for the requested refs.
func titles(m map[string]string) BuildKeywordDocuments {
	return func(_ context.Context, _, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		var out []search.KeywordDocument
		for _, ref := range refs {
			if title, ok := m[ref.ContentID]; ok {
				out = append(out, search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: ref, Language: lang}, Title: title})
			}
		}
		return out, nil
	}
}

func workerOptions(pool *pgxpool.Pool, schema string, build BuildKeywordDocuments) Options {
	return Options{Pool: pool, Schema: schema, Tenant: tenant, SupportedLanguages: []string{"en"}, ContentKinds: []string{"gallery"}, BuildKeywordDocuments: build,
		ListContent: func(context.Context, string, string, string, string, int) ([]contentref.ContentRef, string, bool, error) {
			return nil, "", true, nil
		}}
}

// markDirty is a host write with a fixed timestamp: the queue trigger, not
// updated_at, must fence generations.
func markDirty(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	_, err := pool.Exec(ctx, `INSERT INTO `+schema+`.content_search_dirty(tenant_id,content_kind,content_id,content_version_id,language,updated_at) VALUES($1,'gallery',$2,'','en','2020-01-01')
 ON CONFLICT(tenant_id,content_kind,content_id,content_version_id,language) DO UPDATE SET updated_at=EXCLUDED.updated_at`, tenant, cid(1))
	return err
}

func document(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, "SELECT raw_document FROM "+schema+".content_search_documents WHERE tenant_id=$1 AND content_id=$2", tenant, cid(1)).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+schema+"."+table+" WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegrationDirtyUpdateSurvivesConcurrentSync(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	build := func(ctx context.Context, tenant, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return titles(map[string]string{cid(1): "old"})(ctx, tenant, kind, lang, refs)
		}
		return titles(map[string]string{cid(1): "new"})(ctx, tenant, kind, lang, refs)
	}
	opts := workerOptions(pool, schema, build)
	done := make(chan error, 1)
	go func() { done <- SyncOnce(ctx, opts) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// An equal-timestamp host update commits while the callback is in flight.
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 1 {
		t.Fatal("concurrent equal-timestamp update lost")
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "true"); n != 0 {
		t.Fatal("changed generation published stale document")
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "new" {
		t.Fatalf("document=%q", got)
	}
}

// A failed backfill page records its error without undoing the committed
// dirty batch, and the next tick resumes from the same cursor.
func TestIntegrationBackfillFailureKeepsCursor(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	opts := workerOptions(pool, schema, titles(map[string]string{cid(1): "new", cid(2): "listed"}))
	var cursors []string
	opts.ListContent = func(_ context.Context, _, _, _, cursor string, _ int) ([]contentref.ContentRef, string, bool, error) {
		cursors = append(cursors, cursor)
		if len(cursors) == 1 {
			return []contentref.ContentRef{gallery(cid(2))}, "c1", false, nil
		}
		if len(cursors) == 2 {
			return nil, "", false, errors.New("backfill failed")
		}
		return nil, "", true, nil
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if err := SyncOnce(ctx, opts); err == nil {
		t.Fatal("expected backfill failure")
	}
	if got := document(t, ctx, pool, schema); got != "new" {
		t.Fatal(got)
	}
	if n := count(t, ctx, pool, schema, "content_search_backfill", "cursor='c1' AND state='failed' AND last_error='backfill failed'"); n != 1 {
		t.Fatal("failure not recorded at the committed cursor")
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "content_id='"+cid(2)+"'"); n != 1 {
		t.Fatal("committed page not published")
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cursors) != "[ c1 c1]" {
		t.Fatalf("cursors: %q", cursors)
	}
}

func TestIntegrationStaleWriterCannotOverwrite(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	// A writer stalled in its callback must not publish after an overlapping
	// tick publishes the newer generation.
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	build := func(ctx context.Context, tenant, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return titles(map[string]string{cid(1): "stale"})(ctx, tenant, kind, lang, refs)
		}
		return titles(map[string]string{cid(1): "fresh"})(ctx, tenant, kind, lang, refs)
	}
	opts := workerOptions(pool, schema, build)
	opts.ListContent = func(context.Context, string, string, string, string, int) ([]contentref.ContentRef, string, bool, error) {
		return []contentref.ContentRef{gallery(cid(1))}, "1", true, nil
	}
	done := make(chan error, 1)
	go func() { done <- SyncOnce(ctx, opts) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "fresh" {
		t.Fatalf("stale writer overwrote: %q", got)
	}
}

func TestIntegrationBackfillQueuesSameWriter(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	var calls atomic.Int32
	opts := workerOptions(pool, schema, func(ctx context.Context, tenant, kind, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		calls.Add(1)
		return titles(map[string]string{cid(1): "fresh"})(ctx, tenant, kind, lang, refs)
	})
	opts.ListContent = func(_ context.Context, gotTenant, kind, lang, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
		if gotTenant != tenant || kind != "gallery" || lang != "en" {
			return nil, "", false, fmt.Errorf("unexpected page %s/%s/%s", gotTenant, kind, lang)
		}
		return []contentref.ContentRef{gallery(cid(1)), gallery(cid(1)).WithVersion("v2")}, "1", true, nil
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("backfill bypassed dirty writer")
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "tenant_id=$1 AND reason='backfill'", tenant); n != 2 {
		t.Fatalf("backfill queued %d rows", n)
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "fresh" {
		t.Fatal(got)
	}
	// Both the work and its version document are keyed separately.
	if n := count(t, ctx, pool, schema, "content_search_documents", "tenant_id=$1 AND content_id=$2", tenant, cid(1)); n != 2 {
		t.Fatalf("documents=%d", n)
	}
	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM "+schema+".content_search_backfill WHERE tenant_id=$1 AND content_kind='gallery' AND language='en'", tenant).Scan(&state); err != nil || state != "done" {
		t.Fatalf("backfill state=%q err=%v", state, err)
	}
}

func TestIntegrationDirtyRevisionNeverReusesGeneration(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	var previous int64
	for i := 0; i < 3; i++ {
		if i == 2 {
			if _, err := pool.Exec(ctx, "DELETE FROM "+schema+".content_search_dirty"); err != nil {
				t.Fatal(err)
			}
		}
		if err := markDirty(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
		var revision int64
		if err := pool.QueryRow(ctx, "SELECT revision FROM "+schema+".content_search_dirty").Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if revision <= previous {
			t.Fatalf("generation reused: %d after %d", revision, previous)
		}
		previous = revision
	}
}

// recordingSink captures deliveries and fails while failing is set.
type recordingSink struct {
	mu      sync.Mutex
	failing bool
	upserts []search.PublishedDocument
	deletes []search.DocumentKey
}

func (s *recordingSink) Upsert(_ context.Context, doc search.PublishedDocument) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("sink down")
	}
	s.upserts = append(s.upserts, doc)
	return nil
}

func (s *recordingSink) Delete(_ context.Context, key search.DocumentKey, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("sink down")
	}
	s.deletes = append(s.deletes, key)
	return nil
}

// The DocumentSink port: every published document is delivered with its
// queue revision as Version; a failing sink never blocks the keyword index
// and its row is re-delivered later with a higher Version; deletions reach it.
func TestIntegrationDocumentSinkAtLeastOnce(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	sink := &recordingSink{}
	docs := map[string]string{cid(1): "Blue Ocean"}
	opts := workerOptions(pool, schema, titles(docs))
	opts.Sink = sink
	mark := func(deleted bool) {
		t.Helper()
		if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{DocumentKey: search.DocumentKey{ContentRef: gallery(cid(1)), Language: "en"}, Deleted: deleted}}); err != nil {
			t.Fatal(err)
		}
	}
	revision := func() int64 {
		t.Helper()
		var r int64
		if err := pool.QueryRow(ctx, "SELECT revision FROM "+schema+".content_search_dirty WHERE tenant_id=$1", tenant).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	mark(false)
	first := revision()
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(sink.upserts) != 1 || sink.upserts[0].Version != first || sink.upserts[0].Title != "Blue Ocean" || !sink.upserts[0].Equal(gallery(cid(1))) || sink.upserts[0].Language != "en" {
		t.Fatalf("delivery: %+v", sink.upserts)
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 0 {
		t.Fatal("delivered row must be acknowledged")
	}

	// Sink outage: the document still commits, the row stays queued.
	docs[cid(1)] = "Red Forest"
	sink.failing = true
	mark(false)
	failed := revision()
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := document(t, ctx, pool, schema); got != "Red Forest" {
		t.Fatalf("keyword index must not wait for the sink: %q", got)
	}
	var reason string
	if err := pool.QueryRow(ctx, "SELECT reason FROM "+schema+".content_search_dirty WHERE tenant_id=$1", tenant).Scan(&reason); err != nil || reason != "sink_retry" {
		t.Fatalf("row must stay queued for the sink: reason=%q err=%v", reason, err)
	}
	retry := revision()
	if retry <= failed {
		t.Fatalf("retry must carry a new revision: %d after %d", retry, failed)
	}
	if len(sink.upserts) != 1 {
		t.Fatalf("failed delivery recorded: %+v", sink.upserts)
	}

	// Recovery: re-delivered with the newer version, then acknowledged.
	sink.failing = false
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(sink.upserts) != 2 || sink.upserts[1].Version != retry || sink.upserts[1].Title != "Red Forest" {
		t.Fatalf("re-delivery: %+v", sink.upserts)
	}
	if n := count(t, ctx, pool, schema, "content_search_dirty", "true"); n != 0 {
		t.Fatal("re-delivered row must be acknowledged")
	}

	// Deletion reaches the sink; a builder omitting the content deletes too.
	mark(true)
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	delete(docs, cid(1))
	mark(false)
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(sink.deletes) != 2 || !sink.deletes[0].Equal(gallery(cid(1))) || sink.deletes[0].Language != "en" {
		t.Fatalf("deletes: %+v", sink.deletes)
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "true"); n != 0 {
		t.Fatal("deleted document remains")
	}
}

// A builder that answers outside the requested tenant/kind/language is a
// contract violation, not a silent cross-tenant write.
func TestIntegrationBuilderMustStayInScope(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if err := markDirty(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	opts := workerOptions(pool, schema, func(_ context.Context, _, _, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		return []search.KeywordDocument{{DocumentKey: search.DocumentKey{ContentRef: contentref.New("hentai0", "gallery", cid(1)), Language: lang}, Title: "leak"}}, nil
	})
	if err := SyncOnce(ctx, opts); err == nil {
		t.Fatal("foreign-tenant document accepted")
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "true"); n != 0 {
		t.Fatal("foreign document written")
	}
}

func TestIntegrationInvalidDocumentDoesNotBlockDirtyQueue(t *testing.T) {
	ctx, pool, schema := workerFixture(t)
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.content_search_dirty
 (tenant_id,content_kind,content_id,content_version_id,language,updated_at)
 VALUES ($1,'gallery',$2,'','en','2020-01-01'),
        ($1,'gallery',$3,'','en','2020-01-02')`, tenant, cid(1), cid(2)); err != nil {
		t.Fatal(err)
	}

	invalid := true
	badBuilds := 0
	opts := workerOptions(pool, schema, func(_ context.Context, _, _, lang string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		var docs []search.KeywordDocument
		for _, ref := range refs {
			doc := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: ref, Language: lang}, Title: "valid"}
			if ref.ContentID == cid(1) {
				badBuilds++
				if invalid {
					doc.Keywords = make([]string, 257)
					for i := range doc.Keywords {
						doc.Keywords[i] = fmt.Sprintf("term-%d", i)
					}
				}
			}
			docs = append(docs, doc)
		}
		return docs, nil
	})

	var tickErrors []error
	for range 2 {
		if err := SyncOnce(ctx, opts); err != nil {
			tickErrors = append(tickErrors, err)
		}
	}
	if len(tickErrors) > 0 {
		t.Fatalf("two ticks returned %d errors: %v", len(tickErrors), tickErrors)
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "tenant_id=$1 AND content_id=$2", tenant, cid(2)); n != 1 {
		t.Fatalf("valid document indexed %d times, want 1", n)
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "tenant_id=$1 AND content_id=$2", tenant, cid(1)); n != 0 {
		t.Fatalf("invalid document indexed %d times", n)
	}
	if badBuilds != 1 {
		t.Fatalf("invalid document rebuilt %d times, want 1", badBuilds)
	}
	var failure string
	if err := pool.QueryRow(ctx, "SELECT last_error FROM "+schema+".content_search_invalid WHERE tenant_id=$1 AND content_id=$2", tenant, cid(1)).Scan(&failure); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failure, "exceeds keyword input limits") {
		t.Fatalf("invalid document error = %q", failure)
	}

	invalid = false
	if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{
		DocumentKey: search.DocumentKey{ContentRef: gallery(cid(1)), Language: "en"},
		Reason:      "repaired",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := SyncOnce(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, pool, schema, "content_search_documents", "tenant_id=$1 AND content_id=$2", tenant, cid(1)); n != 1 {
		t.Fatalf("repaired document indexed %d times, want 1", n)
	}
	if n := count(t, ctx, pool, schema, "content_search_invalid", "tenant_id=$1 AND content_id=$2", tenant, cid(1)); n != 0 {
		t.Fatalf("repaired document retains %d invalid records", n)
	}
}
