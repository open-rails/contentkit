package contentkit

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

func TestPersonalizedSearchPagesCompleteCandidateWindows(t *testing.T) {
	ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 262144}))
	pool := testPG(t)
	schema := keywordSchema(t, ctx, pool)
	env := signaltest.FromEnv(t)
	database := "ck_search_window_" + strings.ReplaceAll(contentref.NewID(), "-", "")
	conn := env.Fresh(t, database)
	t.Cleanup(func() { env.Drop(t, conn, database) })
	hub, err := NewEmbedded(EmbeddedConfig{PG: pool, PGSchema: schema, CH: conn, CHDatabase: database, Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	var documents []KeywordDocument
	for n := 1; n <= 600; n++ {
		language := "es"
		if n > 300 {
			language = "en"
		}
		document := doc("gallery", cid(n), language, "Windowedneedle", nil, nil)
		document.ContentRef = document.ContentRef.WithVersion(fmt.Sprintf("edition-%d", n))
		documents = append(documents, document)
	}
	upsertDocs(t, ctx, pool, schema, documents...)
	if err := hub.RecordSignals(ctx, []signal.Signal{{ContentRef: gallery(cid(600)), Subject: signal.Subject{AnonKey: "popular-reader"}, Type: signal.TypeView, EventID: "late-candidate", OccurredAt: time.Now(), Progress: 1, ProgressMax: 1}}); err != nil {
		t.Fatal(err)
	}
	opts := HubSearchOptions{
		SearchOptions: SearchOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: []string{"gallery"}, Limit: 10, CandidateLimit: 400},
		Personalize:   &Personalization{Subject: signal.Subject{UserID: "reader"}, PopularityWeight: 2, DemoteSeen: true, PopularityWindow: signal.AllTime()},
	}
	first, err := hub.Search(ctx, "Windowedneedle", opts)
	if err != nil || len(first.Hits) != 10 || first.Hits[0].ContentID != cid(600) || first.Hits[0].Version() != "edition-600" || first.Hits[0].Language != "en" || first.Truncated {
		t.Fatalf("late fallback candidate must compete across both complete language windows: %+v, %v", first, err)
	}
	opts.Limit, opts.CandidateLimit = 600, 1000
	full, err := hub.Search(ctx, "Windowedneedle", opts)
	if err != nil || len(full.Hits) != 600 || full.HasMore || full.Truncated {
		t.Fatalf("complete ranked window: len=%d, more=%v, truncated=%v, err=%v", len(full.Hits), full.HasMore, full.Truncated, err)
	}
	opts.Limit = 50
	for _, offset := range []int{0, 500, 550, 600} {
		opts.Offset = offset
		page, err := hub.Search(ctx, "Windowedneedle", opts)
		end := min(offset+opts.Limit, len(full.Hits))
		if err != nil || !reflect.DeepEqual(page.Hits, full.Hits[offset:end]) || page.HasMore != (end < len(full.Hits)) || page.Truncated {
			t.Fatalf("page %d disagrees with complete ranking: len=%d, more=%v, truncated=%v, err=%v", offset, len(page.Hits), page.HasMore, page.Truncated, err)
		}
	}
	var dense []KeywordDocument
	for n := 1; n <= 300; n++ {
		document := doc("gallery", cid(1001), "es", "Truncatedneedle", nil, nil)
		document.ContentRef = document.ContentRef.WithVersion(fmt.Sprintf("dense-%d", n))
		dense = append(dense, document)
	}
	upsertDocs(t, ctx, pool, schema, dense...)
	opts.Limit, opts.Offset, opts.CandidateLimit = 1, 1, 20
	truncated, err := hub.Search(ctx, "Truncatedneedle", opts)
	if err != nil || len(truncated.Hits) != 0 || !truncated.HasMore || !truncated.Truncated {
		t.Fatalf("an exhausted truncated window must not claim catalog exhaustion: %+v, %v", truncated, err)
	}
	var bulk []KeywordDocument
	for n := 1; n <= 7001; n++ {
		document := doc("gallery", cid(10000+n), "en", "Bulkneedle", nil, nil)
		document.ContentRef = document.ContentRef.WithVersion(fmt.Sprintf("bulk-edition-%d", n))
		bulk = append(bulk, document)
	}
	upsertDocs(t, ctx, pool, schema, bulk...)
	if err := hub.RecordSignals(ctx, []signal.Signal{{ContentRef: gallery(cid(17001)), Subject: signal.Subject{UserID: "reader"}, Type: signal.TypeView, EventID: "bulk-late-candidate", OccurredAt: time.Now(), Progress: 1, ProgressMax: 1, Completed: true}}); err != nil {
		t.Fatal(err)
	}
	p := *opts.Personalize
	p.DemoteSeen = false
	bulkOpts := HubSearchOptions{
		SearchOptions: SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, Limit: 10, CandidateLimit: 8000},
		Personalize:   &p,
	}
	popular, err := hub.Search(ctx, "Bulkneedle", bulkOpts)
	if err != nil || len(popular.Hits) != 10 || popular.Hits[0].ContentID != cid(17001) || popular.Hits[0].Version() != "bulk-edition-7001" || popular.Truncated {
		t.Fatalf("large-window popularity must not silently fall back to content order: %+v, %v", popular, err)
	}
	p.DemoteSeen = true
	demoted, err := hub.Search(ctx, "Bulkneedle", bulkOpts)
	if err != nil || len(demoted.Hits) != 10 || demoted.Hits[0].ContentID != cid(17001) || math.Abs(float64(demoted.Hits[0].Score-popular.Hits[0].Score*0.6)) > 1e-6 {
		t.Fatalf("large-window completed state must affect the matched edition: %+v, %v", demoted, err)
	}
	if err := conn.Exec(ctx, "DROP TABLE "+database+".subject_content_state"+env.OnCluster()+" SYNC"); err != nil {
		t.Fatal(err)
	}
	opts.Limit, opts.Offset, opts.CandidateLimit = 50, 550, 1000
	plain, err := hub.client.Search(ctx, "Windowedneedle", opts.SearchOptions)
	if err != nil {
		t.Fatal(err)
	}
	degraded, err := hub.Search(ctx, "Windowedneedle", opts)
	if err != nil || !reflect.DeepEqual(degraded, plain) {
		t.Fatalf("signal failure must page the original complete content ranking: %+v, want %+v, err=%v", degraded, plain, err)
	}
}
