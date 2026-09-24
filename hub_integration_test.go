package contentkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

// Round-trip integration test for the embedded hub against a real Postgres
// AND ClickHouse. Opt-in: requires both CONTENTKIT_TEST_URL (PG DSN) and
// CONTENTKIT_TEST_CH_ADDR (CH native addr).
const hubTestCHDB = "contentkit_hub_test"

func TestHubIntegrationRoundTrip(t *testing.T) {
	pool := testPG(t)
	chEnv := signaltest.FromEnv(t)
	ctx := context.Background()
	schema := keywordSchema(t, ctx, pool)
	upsertDocs(t, ctx, pool, schema,
		doc("gallery", cid(1), "en", "two factor authentication guide", nil, nil),
		doc("gallery", cid(2), "en", "two factor backup codes", nil, nil),
		doc("gallery", cid(3), "en", "cooking with cast iron", nil, nil),
	)
	ch := chEnv.Fresh(t, hubTestCHDB)
	if err := signal.CheckSchema(ctx, ch, hubTestCHDB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { chEnv.Drop(t, chEnv.Open(t, ""), hubTestCHDB) })

	hub, err := NewEmbedded(EmbeddedConfig{
		PG:         pool,
		PGSchema:   schema,
		CH:         ch,
		CHDatabase: hubTestCHDB,
		Tenant:     testTenant,
		Scorers: map[string]signal.Scorer{
			"gallery": signal.ScorerFunc(func(_ context.Context, s signal.Signal) (signal.Scored, error) {
				score := int16(0)
				if s.ProgressMax > 0 {
					score = int16(100 * s.Progress / s.ProgressMax)
				}
				return signal.Scored{
					Score:       score,
					Progress:    s.Progress,
					ProgressMax: s.ProgressMax,
					Completed:   s.ProgressMax > 0 && 10*s.Progress >= 9*s.ProgressMax,
				}, nil
			}),
		},
		Catalogs: map[string]ContentCatalog{
			"gallery": ContentCatalogFunc(func(context.Context, string, string, CatalogQuery) ([]string, error) {
				return []string{cid(3), cid(2), cid(1)}, nil // newest first
			}),
		},
	})
	if err != nil {
		t.Fatalf("NewEmbedded: %v", err)
	}
	var _ Hub = hub

	user := signal.Subject{UserID: "u1"}
	g1, g2 := hub.Content("gallery", cid(1)), hub.Content("gallery", cid(2))
	day := func(d, h int) time.Time { return time.Date(2026, 6, d, h, 0, 0, 0, time.UTC) }

	// Record signals: u1 completes g1; many anons view g2 (popular); a0 views
	// both so g2 is co-engaged with g1.
	g1View := signal.Signal{ContentRef: g1, Subject: user, Type: signal.TypeView, EventID: "u1-g1", OccurredAt: day(1, 10), Progress: 20, ProgressMax: 20, Resume: "p:20"}
	batch := []signal.Signal{g1View}
	for i := 0; i < 8; i++ {
		batch = append(batch, signal.Signal{ContentRef: g2, Subject: signal.Subject{AnonKey: fmt.Sprintf("a%d", i)}, Type: signal.TypeView,
			EventID: fmt.Sprintf("a%d-g2", i), OccurredAt: day(2, 9+i%6), Progress: 18, ProgressMax: 20})
	}
	batch = append(batch, signal.Signal{ContentRef: g1, Subject: signal.Subject{AnonKey: "a0"}, Type: signal.TypeView, EventID: "a0-g1", OccurredAt: day(2, 9), Progress: 18, ProgressMax: 20})
	if err := hub.RecordSignals(ctx, batch); err != nil {
		t.Fatalf("RecordSignals: %v", err)
	}

	// Scorer applied: g1 state must be completed with score 100.
	states, err := hub.States(ctx, user, []ContentRef{g1})
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if s := states[g1.Key()]; !s.Completed || s.LastScore != 100 || s.Resume != "p:20" {
		t.Fatalf("scorer not applied: %+v", s)
	}

	// History.
	hist, err := hub.History(ctx, user, signal.HistoryOptions{ContentKind: "gallery"})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 1 || !hist[0].Equal(g1) {
		t.Fatalf("history: %+v", hist)
	}

	// Unseen: u1 saw g1 -> g3, g2 remain (catalog order).
	unseen, err := hub.Unseen(ctx, user, UnseenOptions{ContentKind: "gallery"})
	if err != nil {
		t.Fatalf("Unseen: %v", err)
	}
	if len(unseen) != 2 || unseen[0] != cid(3) || unseen[1] != cid(2) {
		t.Fatalf("unseen: %v", unseen)
	}

	// Named metrics on g2.
	metrics, err := hub.Metrics(ctx, []ContentRef{g2}, signal.AllTime())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m := metrics[g2.Key()]; m.AnonViewers != 8 || m.Views != 8 {
		t.Fatalf("metrics: %+v", m)
	}

	// Popular: g2 must lead (8 subjects vs 2).
	pop, err := hub.Popular(ctx, "gallery", signal.PopularOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Popular: %v", err)
	}
	if len(pop) < 2 || !pop[0].Equal(g2) {
		t.Fatalf("popular: %+v", pop)
	}

	// Plain search: content ranking only.
	plain, err := hub.Search(ctx, "two factor", HubSearchOptions{SearchOptions: SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, Limit: 10}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(plain.Hits) < 2 {
		t.Fatalf("plain search: %+v", plain)
	}

	// Personalized search: popularity blend + demote-seen. g1 is completed
	// by u1 and g2 is popular, so g2 must outrank g1.
	pers, err := hub.Search(ctx, "two factor", HubSearchOptions{
		SearchOptions: SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, Limit: 10},
		Personalize:   &Personalization{Subject: user, PopularityWeight: 1, DemoteSeen: true, PopularityWindow: signal.AllTime()},
	})
	if err != nil {
		t.Fatalf("personalized Search: %v", err)
	}
	rank := map[string]int{}
	for i, h := range pers.Hits {
		rank[h.ContentID] = i
	}
	if rank[cid(2)] > rank[cid(1)] {
		t.Fatalf("personalization must rank popular g2 above completed g1: %+v", pers)
	}

	// SimilarTo: co-engagement from g1 yields g2 (a0 read both), never the anchor.
	sim, err := hub.SimilarTo(ctx, g1, SimilarOptions{})
	if err != nil {
		t.Fatalf("SimilarTo: %v", err)
	}
	if len(sim) != 1 || !sim[0].Equal(g2) {
		t.Fatalf("similar: %+v", sim)
	}

	// Recommend: u1's seed is g1 -> co-engagement suggests g2; g1 (seen) must
	// never be recommended.
	recs, err := hub.Recommend(ctx, user, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 5})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(recs) == 0 || !recs[0].Equal(g2) {
		t.Fatalf("g2 should lead recs: %+v", recs)
	}
	for _, r := range recs {
		if r.Equal(g1) {
			t.Fatalf("seen work recommended: %+v", recs)
		}
	}

	// Replay idempotency through the hub: re-record u1's g1 session.
	if err := hub.RecordSignals(ctx, []signal.Signal{g1View}); err != nil {
		t.Fatalf("replay RecordSignals: %v", err)
	}
	states, err = hub.States(ctx, user, []ContentRef{g1})
	if err != nil {
		t.Fatalf("States after replay: %v", err)
	}
	if got := states[g1.Key()]; got.TotalEvents != 1 {
		t.Fatalf("replay double-counted: %+v", got)
	}
}
