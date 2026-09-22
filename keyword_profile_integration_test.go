package contentkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/worker"
)

// The complete baseline supports the keyword worker and preserves its ledger.
func TestKeywordProfileIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := profileDB(t, ctx, "ck_profile", nil)
	applyLineage(t, ctx, pool, migrations.Postgres, nil)
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, "SELECT string_agg(name||':'||checksum,',' ORDER BY name) FROM public.applied_migrations").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ledger := snapshot()
	var tables, columns, embedding int
	if err := pool.QueryRow(ctx, "SELECT count(DISTINCT table_name),count(*) FROM information_schema.columns WHERE table_schema='app' AND table_name IN ('content_search_documents','content_search_dirty','content_search_backfill')").Scan(&tables, &columns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='app' AND (table_name LIKE 'embedding%' OR table_name LIKE 'search\\_%')").Scan(&embedding); err != nil {
		t.Fatal(err)
	}
	if tables != 3 || columns != 28 || embedding != 0 {
		t.Fatalf("schema=%d tables/%d columns, %d pre-ContentKit tables; want 3/28/0", tables, columns, embedding)
	}
	client, err := NewClient(ClientConfig{Pool: pool, Schema: "app", Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	count := func(table, where string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM app."+table+" WHERE "+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expectHits := func(query string, want int) {
		t.Helper()
		page, err := client.Search(ctx, query, SearchOptions{ContentKinds: []string{"gallery"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != want {
			t.Fatalf("%q got %v want %d hits", query, page, want)
		}
	}

	documents := map[string]string{"1": "Blue ocean"}
	var buildErr error
	opts := worker.Options{Pool: pool, Schema: "app", Tenant: testTenant, SupportedLanguages: []string{"en"}, ContentKinds: []string{"gallery"},
		ListContent: func(context.Context, string, string, string, string, int) ([]ContentRef, string, bool, error) {
			return nil, "", true, nil
		},
		BuildKeywordDocuments: func(_ context.Context, _, kind, lang string, refs []ContentRef) ([]KeywordDocument, error) {
			if buildErr != nil {
				return nil, buildErr
			}
			var out []KeywordDocument
			for _, ref := range refs {
				if title, ok := documents[ref.ContentID]; ok {
					out = append(out, KeywordDocument{DocumentKey: DocumentKey{ContentRef: ref, Language: lang}, Title: title, Aliases: []string{"azure waves"}})
				}
			}
			return out, nil
		}}
	mark := func(deleted bool) {
		t.Helper()
		if err := search.MarkDirty(ctx, pool, "app", []search.DirtyMark{{DocumentKey: DocumentKey{ContentRef: gallery("1"), Language: "en"}, Deleted: deleted}}); err != nil {
			t.Fatal(err)
		}
		if err := worker.SyncOnce(ctx, opts); err != nil {
			t.Fatal(err)
		}
	}
	mark(false)
	expectHits("Blue ocean", 1)
	expectHits("azure", 1)
	buildErr = errors.New("temporary source failure")
	if err := search.MarkDirty(ctx, pool, "app", []search.DirtyMark{{DocumentKey: DocumentKey{ContentRef: gallery("1"), Language: "en"}, Reason: "retry"}}); err != nil {
		t.Fatal(err)
	}
	if err := worker.SyncOnce(ctx, opts); !errors.Is(err, buildErr) {
		t.Fatalf("expected builder failure, got %v", err)
	}
	expectHits("Blue ocean", 1)
	if count("content_search_dirty", "true") != 1 {
		t.Fatal("failed build lost pending retry")
	}
	buildErr = nil
	documents["1"] = "Red forest"
	mark(false)
	expectHits("Red forest", 1)
	expectHits("Blue ocean", 0)
	// Omitting a requested reference means the content no longer exists.
	delete(documents, "1")
	mark(false)
	expectHits("Red forest", 0)
	documents["1"] = "Amber field"
	mark(false)
	expectHits("Amber field", 1)
	mark(true)
	expectHits("Amber field", 0)
	if snapshot() != ledger {
		t.Fatal("keyword runtime modified the migration ledger")
	}
}
