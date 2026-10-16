package signal

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// A many-core server must not allocate an aggregation pipeline per CPU for
// this small request. The same 256 MiB budget applies regardless of its default
// parallelism; a higher limit would hide the regression seen in development.
func TestNormalizedPopularityManyCoreMemoryBudget(t *testing.T) {
	store, _ := queryLimitStore(t)
	ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{
		"max_threads":      32,
		"max_memory_usage": 268435456,
	}))
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	ref := gallery("many_core", cid(1))
	var population []PopulationObservation
	var views []Signal
	for i := range 40 {
		subject := Subject{UserID: fmt.Sprintf("reader-%d", i)}
		population = append(population, PopulationObservation{
			ID: fmt.Sprintf("population-%d", i), ContentKind: ref.ContentKind,
			Subject: subject, ObservedAt: start.Add(time.Minute),
			Domain: "main", Language: "en", EligibilityKnown: true,
		})
		views = append(views, Signal{
			ContentRef: ref, Subject: subject, Type: TypeView, EventID: fmt.Sprintf("view-%d", i),
			OccurredAt: start.Add(time.Minute), Progress: 1, ProgressMax: 10, Score: 10,
			Access: &AccessContext{ObservedAt: start.Add(time.Minute), ContentAccess: "public",
				ViewerAccess: "full", Audience: "public", PolicyRevision: "fixture",
				EligibilityKnown: true, Domain: "main", Language: "en"},
		})
	}
	if err := store.RecordPopulation(ctx, ref.TenantID, population); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSignals(ctx, ref.TenantID, views); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Popular(ctx, ref.TenantID, ref.ContentKind, PopularOptions{
		Window: Between(start, end), Limit: 6, RankExpr: "normalized_rate",
		Normalization: &NormalizationOptions{Availability: Availability{Known: true, From: start, Until: end,
			Intervals: []AccessInterval{{ID: "public", ContentID: ref.ContentID, From: start, Until: end, Audience: "public"}}},
			Scope: ActivityScope{Domain: "main", Language: "en"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ContentID != ref.ContentID || math.Abs(hits[0].Score-40.0/60.0) > 1e-12 {
		t.Fatalf("expected all forty readers and the unchanged smoothed rate, got %+v", hits)
	}
}
