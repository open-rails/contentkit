package contentkit

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
	"github.com/open-rails/contentkit/worker"
)

const isolationCHDB = "contentkit_tenant_isolation_test"

// Two tenants share one Postgres schema and one ClickHouse database. Neither
// can read the other's search documents, dirty rows, backfill state, signals,
// exposures or preferences, and every cross-tenant reference is refused.
func TestTenantIsolationIntegration(t *testing.T) {
	pool := testPG(t)
	chEnv := signaltest.FromEnv(t)
	ctx := context.Background()
	schema := keywordSchema(t, ctx, pool)
	ch := chEnv.Fresh(t, isolationCHDB)
	t.Cleanup(func() { chEnv.Drop(t, chEnv.Open(t, ""), isolationCHDB) })

	const a, b = "doujins", "hentai0"
	hubs := map[string]*EmbeddedHub{}
	for _, tenant := range []string{a, b} {
		h, err := NewEmbedded(EmbeddedConfig{PG: pool, PGSchema: schema, CH: ch, CHDatabase: isolationCHDB, Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		hubs[tenant] = h
	}
	key := func(tenant, id, lang string) DocumentKey {
		return DocumentKey{ContentRef: contentref.New(tenant, "gallery", id), Language: lang}
	}

	// Search documents: the same title in both tenants; each sees only its own.
	upsertDocs(t, ctx, pool, schema,
		KeywordDocument{DocumentKey: key(a, "g1", "en"), Title: "Blue Ocean"},
		KeywordDocument{DocumentKey: key(b, "h1", "en"), Title: "Blue Ocean"},
	)
	for tenant, want := range map[string]string{a: "g1", b: "h1"} {
		page, err := hubs[tenant].Search(ctx, "blue ocean", HubSearchOptions{SearchOptions: SearchOptions{Language: "en", ContentKinds: []string{"gallery"}}})
		if err != nil || len(page.Hits) != 1 || page.Hits[0].ContentID != want || page.Hits[0].TenantID != tenant {
			t.Fatalf("%s search: %+v %v", tenant, page, err)
		}
		suggestions, err := hubs[tenant].Typeahead(ctx, "blue", TypeaheadOptions{Language: "en", ContentKinds: []string{"gallery"}})
		if err != nil || len(suggestions) != 1 || suggestions[0].ContentID != want {
			t.Fatalf("%s typeahead: %+v %v", tenant, suggestions, err)
		}
	}
	// Candidate verification never admits another tenant's document.
	if _, err := search.Eligible(ctx, pool, search.Options{Schema: schema, Tenant: a, Language: "en"}, []search.Candidate{{ContentRef: contentref.New(b, "gallery", "h1"), Language: "en"}}); err == nil {
		t.Fatal("foreign candidate verified")
	}

	// Dirty rows and backfill state: tenant A's worker never touches B's rows.
	if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{DocumentKey: key(a, "g2", "en")}, {DocumentKey: key(b, "h2", "en")}}); err != nil {
		t.Fatal(err)
	}
	var requested []ContentRef
	opts := worker.Options{Pool: pool, Schema: schema, Tenant: a, SupportedLanguages: []string{"en"}, ContentKinds: []string{"gallery"},
		ListContent: func(_ context.Context, tenant, _, _, _ string, _ int) ([]ContentRef, string, bool, error) {
			return []ContentRef{contentref.New(tenant, "gallery", "g3")}, "", true, nil
		},
		BuildKeywordDocuments: func(_ context.Context, tenant, kind, lang string, refs []ContentRef) ([]KeywordDocument, error) {
			var out []KeywordDocument
			for _, ref := range refs {
				if ref.TenantID != tenant {
					t.Fatalf("builder asked for %s outside tenant %s", ref, tenant)
				}
				requested = append(requested, ref)
				out = append(out, KeywordDocument{DocumentKey: DocumentKey{ContentRef: ref, Language: lang}, Title: "Built " + ref.ContentID})
			}
			return out, nil
		}}
	for i := 0; i < 2; i++ {
		if err := worker.SyncOnce(ctx, opts); err != nil {
			t.Fatal(err)
		}
	}
	if len(requested) != 2 || requested[0].ContentID != "g2" || requested[1].ContentID != "g3" {
		t.Fatalf("tenant A worker built %+v", requested)
	}
	var dirtyB, dirtyA, backfillB, docsB int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM "+schema+".content_search_dirty WHERE tenant_id=$1), (SELECT count(*) FROM "+schema+".content_search_dirty WHERE tenant_id=$2), (SELECT count(*) FROM "+schema+".content_search_backfill WHERE tenant_id=$1), (SELECT count(*) FROM "+schema+".content_search_documents WHERE tenant_id=$1)", b, a).Scan(&dirtyB, &dirtyA, &backfillB, &docsB); err != nil {
		t.Fatal(err)
	}
	if dirtyB != 1 || dirtyA != 0 || backfillB != 0 || docsB != 1 {
		t.Fatalf("tenant B queue touched: dirtyB=%d dirtyA=%d backfillB=%d docsB=%d", dirtyB, dirtyA, backfillB, docsB)
	}

	// Signals, exposures and preferences: one subject in both tenants.
	u := signal.Subject{UserID: "shared-account"}
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	for _, tenant := range []string{a, b} {
		h := hubs[tenant]
		g := h.Content("gallery", "g1")
		if err := h.RecordSignals(ctx, []signal.Signal{
			{ContentRef: g, Subject: u, Type: signal.TypeView, EventID: "v", OccurredAt: at, Progress: 10, ProgressMax: 10, Score: 50, Completed: true},
			{ContentRef: h.Content("gallery", "g2"), Subject: signal.Subject{AnonKey: "x"}, Type: signal.TypeView, EventID: "v2", OccurredAt: at, Progress: 10, ProgressMax: 10, Score: 50, Completed: true},
			{ContentRef: h.Content("gallery", "g2"), Subject: u, Type: signal.TypeView, EventID: "v3", OccurredAt: at, Progress: 10, ProgressMax: 10, Score: 50, Completed: true},
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.RecordExposures(ctx, []signal.Exposure{{RenderID: "r-" + tenant, Stage: signal.StageRendered, Subject: u, OccurredAt: at, Shown: []signal.Placement{{ContentRef: g, Position: 1}}}}); err != nil {
			t.Fatal(err)
		}
	}
	// A dislike only in A.
	if err := hubs[a].RecordSignals(ctx, []signal.Signal{{ContentRef: hubs[a].Content("gallery", "g1"), Subject: u, Type: "reaction", EventID: "pref", OccurredAt: at, Value: -1}}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{a, b} {
		h := hubs[tenant]
		g1 := h.Content("gallery", "g1")
		hist, err := h.History(ctx, u, signal.HistoryOptions{})
		if err != nil || len(hist) != 2 || hist[0].TenantID != tenant {
			t.Fatalf("%s history: %+v %v", tenant, hist, err)
		}
		m, err := h.Metrics(ctx, []ContentRef{g1}, signal.AllTime())
		if err != nil || m[g1.Key()].Viewers != 1 {
			t.Fatalf("%s metrics: %+v %v", tenant, m, err)
		}
		pop, err := h.Popular(ctx, "gallery", signal.PopularOptions{})
		if err != nil || len(pop) != 2 || pop[0].TenantID != tenant {
			t.Fatalf("%s popular: %+v %v", tenant, pop, err)
		}
		co, err := h.SimilarTo(ctx, g1, SimilarOptions{})
		if err != nil || len(co) != 1 || co[0].TenantID != tenant {
			t.Fatalf("%s co-engagement: %+v %v", tenant, co, err)
		}
		page, err := h.Attribution(ctx, signal.AttributionOptions{Stage: signal.StageRendered})
		if err != nil || len(page.Renders) != 1 || page.Renders[0].RenderID != "r-"+tenant {
			t.Fatalf("%s attribution: %+v %v", tenant, page, err)
		}
	}
	negA, err := hubs[a].store.NegativeIDs(ctx, a, u, nil)
	if err != nil || len(negA) != 1 {
		t.Fatalf("A preferences: %+v %v", negA, err)
	}
	negB, err := hubs[b].store.NegativeIDs(ctx, b, u, nil)
	if err != nil || len(negB) != 0 {
		t.Fatalf("B must not see A's dislike: %+v %v", negB, err)
	}
	// Cross-tenant references are refused everywhere, never silently remapped.
	foreign := hubs[b].Content("gallery", "g1")
	if err := hubs[a].RecordSignals(ctx, []signal.Signal{{ContentRef: foreign, Subject: u, Type: signal.TypeView, EventID: "x", OccurredAt: at}}); err == nil {
		t.Fatal("A recorded a signal for B's content")
	}
	if _, err := hubs[a].States(ctx, u, []ContentRef{foreign}); err == nil {
		t.Fatal("A read state of B's content")
	}
	if err := hubs[a].RecordExposures(ctx, []signal.Exposure{{RenderID: "x", Stage: signal.StageRendered, Subject: u, OccurredAt: at, Shown: []signal.Placement{{ContentRef: foreign, Position: 1}}}}); err == nil {
		t.Fatal("A recorded an exposure of B's content")
	}
	// Erasure in A leaves B intact.
	if report, err := hubs[a].EraseSubjects(ctx, []signal.Subject{u}); err != nil || !report.Complete() {
		t.Fatalf("erase: %+v %v", report, err)
	}
	if hist, err := hubs[b].History(ctx, u, signal.HistoryOptions{}); err != nil || len(hist) != 2 {
		t.Fatalf("B history after A's erasure: %+v %v", hist, err)
	}
}
