package hub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/engagementtest"
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

	hub, err := New(Config{
		PG:         pool,
		PGSchema:   schema,
		CH:         ch,
		CHDatabase: hubTestCHDB,
		Tenant:     testTenant,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	user := signal.Subject{UserID: "u1"}
	g1, g2 := hub.Content("gallery", cid(1)), hub.Content("gallery", cid(2))
	day := func(d, h int) time.Time { return time.Date(2026, 6, d, h, 0, 0, 0, time.UTC) }

	// Explicit measured pages: u1 completes g1; eight guests read g2; a0
	// reads both so g2 is co-engaged with g1. Opens alone are not evidence.
	g1Reading := engagementtest.Observation{ContentRef: g1, Subject: user, Type: engagementtest.TypeReading, EventID: "u1-g1", OccurredAt: day(1, 10), Progress: 20, ProgressMax: 20, DurationS: 160, Resume: "p:20"}
	batch := []engagementtest.Observation{g1Reading}
	for i := 0; i < 8; i++ {
		batch = append(batch, engagementtest.Observation{ContentRef: g2, Subject: signal.Subject{AnonKey: fmt.Sprintf("a%d", i)}, Type: engagementtest.TypeReading,
			EventID: fmt.Sprintf("a%d-g2", i), OccurredAt: day(2, 9+i%6), Progress: 18, ProgressMax: 20, DurationS: 144})
	}
	batch = append(batch, engagementtest.Observation{ContentRef: g1, Subject: signal.Subject{AnonKey: "a0"}, Type: engagementtest.TypeReading, EventID: "a0-g1", OccurredAt: day(2, 9), Progress: 18, ProgressMax: 20, DurationS: 144})
	if err := hub.RecordSignals(ctx, engagementtest.Native(batch)); err != nil {
		t.Fatalf("RecordSignals: %v", err)
	}

	// g1's state is completed with full measured coverage and attention.
	states, err := hub.States(ctx, user, []ContentRef{g1})
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if s := states[g1.Key()]; !s.Completed || s.Quality != 1 || s.ActiveMS != 160_000 || s.Resume != "p:20" {
		t.Fatalf("state: %+v", s)
	}

	// History.
	hist, err := hub.History(ctx, user, signal.HistoryOptions{ContentKind: "gallery"})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 1 || !hist[0].Equal(g1) {
		t.Fatalf("history: %+v", hist)
	}

	// Named metrics on g2.
	metrics, err := hub.Metrics(ctx, []ContentRef{g2}, signal.AllTime())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m := metrics[g2.Key()]; m.GuestReaders != 8 || m.Readers != 8 || m.ActiveMS != 8*144_000 {
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

	// Replay idempotency through the hub: re-record u1's g1 session.
	if err := hub.RecordSignals(ctx, g1Reading.Signals()); err != nil {
		t.Fatalf("replay RecordSignals: %v", err)
	}
	states, err = hub.States(ctx, user, []ContentRef{g1})
	if err != nil {
		t.Fatalf("States after replay: %v", err)
	}
	if got := states[g1.Key()]; got.ActiveMS != 160_000 || got.MaxProgress != 2_000 || got.Quality != 1 {
		t.Fatalf("replay double-counted: %+v", got)
	}
}
