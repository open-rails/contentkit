package popularity_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

type normalizedSource struct{ store *signal.Store }

func (s normalizedSource) Tenant() string { return "normalized" }
func (s normalizedSource) Popular(ctx context.Context, kind string, opts signal.PopularOptions) ([]signal.PopularHit, error) {
	return s.store.Popular(ctx, s.Tenant(), kind, opts)
}
func (s normalizedSource) Metrics(ctx context.Context, refs []signal.ContentRef, window signal.Window) (map[signal.ContentKey]signal.ContentMetrics, error) {
	return s.store.Metrics(ctx, s.Tenant(), refs, window)
}
func (s normalizedSource) PopulationCoverage(ctx context.Context, kind string, scope signal.ActivityScope, window signal.Window) (time.Time, bool, error) {
	return s.store.PopulationCoverage(ctx, s.Tenant(), kind, scope, window)
}

type normalizedAvailability struct{ value signal.Availability }

func (a normalizedAvailability) Availability(context.Context, string, signal.Window, signal.ActivityScope) (signal.Availability, error) {
	return a.value, nil
}

type countingAvailability struct {
	value signal.Availability
	calls int
}

func (a *countingAvailability) Availability(context.Context, string, signal.Window, signal.ActivityScope) (signal.Availability, error) {
	a.calls++
	copy := a.value
	copy.Until = copy.Until.Add(time.Duration(a.calls) * time.Microsecond)
	return copy, nil
}

func TestNormalizedPopularityUsesEventTimePopulationAndQuality(t *testing.T) {
	ctx := context.Background()
	const database = "contentkit_normalization_test"
	conn := signaltest.FromEnv(t).Fresh(t, database)
	store, err := signal.NewStore(conn, database)
	if err != nil {
		t.Fatal(err)
	}
	if err := signal.CheckSchema(ctx, conn, database); err != nil {
		t.Fatal(err)
	}
	source := normalizedSource{store}
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	middle, end := start.AddDate(0, 0, 1), start.AddDate(0, 0, 2)
	free, paid, switched := fxNoVotes, fxOneLike, fxMixed
	availability := signal.Availability{Known: true, From: start, Until: end, Intervals: []signal.AccessInterval{
		{ID: "free", ContentID: free, From: start, Until: end, Audience: "public"},
		{ID: "paid", ContentID: paid, From: start, Until: end, Audience: "premium", AudienceTiers: []string{"premium"}},
		{ID: "switch-public", ContentID: switched, From: start, Until: middle, Audience: "public"},
		{ID: "switch-paid", ContentID: switched, From: middle, Until: end, Audience: "premium", AudienceTiers: []string{"premium"}},
	}}
	ranker, err := popularity.New(popularity.Config{Source: source, Policy: popularity.PolicyV2, Availability: normalizedAvailability{availability}})
	if err != nil {
		t.Fatal(err)
	}
	window := signal.Between(start, end)
	before, err := ranker.PopularResult(ctx, "gallery", window, 10)
	if err != nil || before.Ranking.Policy != "v1" || before.Ranking.Mode != "legacy" {
		t.Fatalf("unobserved history must be explicit legacy: %+v %v", before, err)
	}
	var population []signal.PopulationObservation
	for day := range 2 {
		for i := range 1000 {
			tiers := []string{}
			if i < 100 {
				tiers = []string{"premium"}
			}
			population = append(population, signal.PopulationObservation{ID: fmt.Sprintf("d%d-u%d", day, i), ContentKind: "gallery",
				Subject: signal.Subject{UserID: fmt.Sprintf("u%d", i)}, ObservedAt: start.AddDate(0, 0, day),
				Domain: "main", Language: "en", EligibleTiers: tiers, EligibilityKnown: true})
		}
	}
	// Other content kinds and domains cannot inflate this audience.
	for i := range 100 {
		population = append(population, signal.PopulationObservation{ID: fmt.Sprintf("other%d", i), ContentKind: "blog",
			Subject: signal.Subject{UserID: fmt.Sprintf("other%d", i)}, ObservedAt: start, Domain: "main", Language: "en", EligibilityKnown: true})
	}
	for from := 0; from < len(population); from += signal.MaxSignalsPerBatch {
		if err := store.RecordPopulation(ctx, source.Tenant(), population[from:min(from+signal.MaxSignalsPerBatch, len(population))]); err != nil {
			t.Fatal(err)
		}
	}
	var readings []engagementtest.Observation
	appendReading := func(id string, user int, when time.Time, restricted, preview bool) {
		access, audience, full := "public", "public", "full"
		tiers := []string{}
		if user < 100 {
			tiers = []string{"premium"}
		}
		if restricted {
			access, audience = "restricted", "premium"
		}
		if preview {
			full = "preview"
		}
		readings = append(readings, engagementtest.Observation{ContentRef: contentref.New(source.Tenant(), "gallery", id), Subject: signal.Subject{UserID: fmt.Sprintf("u%d", user)},
			Type: engagementtest.TypeReading, EventID: fmt.Sprintf("%s-%d-%d", id, user, when.Unix()), OccurredAt: when,
			DurationS: 8, Progress: 1, ProgressMax: 1,
			Access: &signal.AccessContext{ObservedAt: when, ContentAccess: access, ViewerAccess: full, Audience: audience,
				Domain: "main", Language: "en", EligibleTiers: tiers, EligibilityKnown: true, PolicyRevision: "historical"}})
	}
	for user := range 100 {
		appendReading(free, user, start.Add(time.Hour), false, false)
		appendReading(switched, user, start.Add(time.Hour), false, false)
	}
	for user := range 10 {
		appendReading(paid, user, start.Add(time.Hour), true, false)
		appendReading(switched, user, middle.Add(time.Hour), true, false)
	}
	for user := 100; user < 120; user++ {
		appendReading(paid, user, start.Add(time.Hour), true, true)
	}
	// The real server reaction carries no browser access payload. Its author
	// has a verified full read in this interval, so approval must still count.
	readings = append(readings, engagementtest.Observation{ContentRef: contentref.New(source.Tenant(), "gallery", free), Subject: signal.Subject{UserID: "u0"},
		Type: "reaction", EventID: "liked", OccurredAt: start.Add(2 * time.Hour), Value: 1})
	if err := engagementtest.Record(ctx, store, source.Tenant(), readings); err != nil {
		t.Fatal(err)
	}
	result, err := ranker.WithScope(signal.ActivityScope{Domain: "main", Language: "en"}).PopularResult(ctx, "gallery", window, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ranking.Policy != "v2" || result.Ranking.Mode != "population_normalized" || len(result.Hits) != 3 {
		t.Fatalf("normalized result %+v", result)
	}
	byID := map[string]popularity.Hit{}
	for _, hit := range result.Hits {
		byID[hit.ContentID] = hit
		if math.Abs(hit.Score-popularity.PolicyV2.Score(hit.ContentMetrics)) > 1e-10 {
			t.Fatalf("SQL/Go score mismatch: %+v", hit)
		}
	}
	close := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s=%g, want %g", name, got, want)
		}
	}
	close("free raw participation", byID[free].Normalization.RawRate, .1)
	close("premium raw participation", byID[paid].Normalization.RawRate, .1)
	close("switch raw participation", byID[switched].Normalization.RawRate, .1)
	close("free reliable rate", byID[free].Normalization.Rate, 100./1020)
	close("premium reliable rate", byID[paid].Normalization.Rate, 10./120)
	close("switch reliable rate", byID[switched].Normalization.Rate, .5*(100./1020+10./120))
	close("real approval", byID[free].Normalization.Approval/byID[free].Normalization.Rate, 4.75/6)
	if byID[paid].Readers != 10 {
		t.Fatalf("previews inflated qualified readers: %+v", byID[paid])
	}
	selected, err := ranker.WithScope(signal.ActivityScope{Domain: "main", Language: "en"}).Candidates(ctx, "gallery", []string{paid}, window)
	if err != nil || len(selected) != 1 || math.Abs(selected[0].Score-byID[paid].Score) > 1e-10 {
		t.Fatalf("candidate normalization drift: %+v %v", selected, err)
	}
	tuned := popularity.PolicyV2
	tuned.Name = "v2-tuned"
	tuned.Weights.Approval = .8
	variant, err := popularity.New(popularity.Config{Source: source, Policy: tuned, Availability: normalizedAvailability{availability}})
	if err != nil {
		t.Fatal(err)
	}
	tunedHits, err := variant.WithScope(signal.ActivityScope{Domain: "main", Language: "en"}).Candidates(ctx, "gallery", []string{paid}, window)
	if err != nil || len(tunedHits) != 1 || tunedHits[0].Normalization == nil || math.Abs(tunedHits[0].Score-tuned.Score(tunedHits[0].ContentMetrics)) > 1e-10 {
		t.Fatalf("renamed normalized policy reverted to legacy: %+v %v", tunedHits, err)
	}
	// A repeated delivery changes no numerator, denominator or quality term.
	if err := engagementtest.Record(ctx, store, source.Tenant(), readings); err != nil {
		t.Fatal(err)
	}
	again, err := ranker.WithScope(signal.ActivityScope{Domain: "main", Language: "en"}).PopularResult(ctx, "gallery", window, 10)
	if err != nil || len(again.Hits) != len(result.Hits) {
		t.Fatalf("replay result: %+v %v", again, err)
	}
	for _, hit := range again.Hits {
		close("replay score", hit.Score, byID[hit.ContentID].Score)
	}
	// A moving availability endpoint must not defeat the bounded cache, and
	// successive page depths must retain the exact same coverage snapshot.
	provider := &countingAvailability{value: availability}
	cached, err := popularity.New(popularity.Config{Source: source, Policy: popularity.PolicyV2,
		Availability: provider, Cache: popularity.NewMemoryCache(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	cached = cached.WithScope(signal.ActivityScope{Domain: "main", Language: "en"})
	one, err := cached.PopularResult(ctx, "gallery", window, 1)
	if err != nil {
		t.Fatal(err)
	}
	two, err := cached.PopularResult(ctx, "gallery", window, 2)
	if err != nil || len(two.Hits) != 2 || provider.calls != 1 || one.Ranking != two.Ranking {
		t.Fatalf("unstable cache: provider calls=%d, first=%+v second=%+v err=%v", provider.calls, one, two, err)
	}
	// The first observation may begin at microsecond precision; truncating its
	// receipt to milliseconds would drop the first legitimate reader.
	micro := start.Add(123456 * time.Microsecond)
	if err := store.RecordPopulation(ctx, source.Tenant(), []signal.PopulationObservation{{ID: "micro", ContentKind: "micro",
		Subject: signal.Subject{UserID: "micro"}, ObservedAt: micro, Domain: "main", Language: "en", EligibilityKnown: true}}); err != nil {
		t.Fatal(err)
	}
	if err := engagementtest.Record(ctx, store, source.Tenant(), []engagementtest.Observation{{ContentRef: contentref.New(source.Tenant(), "micro", fxLate),
		Subject: signal.Subject{UserID: "micro"}, Type: engagementtest.TypeReading, EventID: "micro", OccurredAt: micro, DurationS: 8, Progress: 1, ProgressMax: 1,
		Access: &signal.AccessContext{ObservedAt: micro, ContentAccess: "public", ViewerAccess: "full", Audience: "public",
			EligibilityKnown: true, Domain: "main", Language: "en"}}}); err != nil {
		t.Fatal(err)
	}
	microHits, err := store.Popular(ctx, source.Tenant(), "micro", signal.PopularOptions{Window: window, RankExpr: popularity.PolicyV2.RankExpr(),
		Normalization: &signal.NormalizationOptions{Availability: signal.Availability{Known: true, From: micro, Until: middle,
			Intervals: []signal.AccessInterval{{ID: "micro", ContentID: fxLate, From: micro, Until: middle, Audience: "public"}}},
			Scope: signal.ActivityScope{Domain: "main", Language: "en"}}})
	if err != nil || len(microHits) != 1 || math.Abs(microHits[0].Normalization.Rate-1./21) > 1e-10 {
		t.Fatalf("first microsecond observation excluded: %+v %v", microHits, err)
	}
	if report, err := store.EraseSubjects(ctx, []string{source.Tenant()}, []signal.Subject{{UserID: "u0"}}); err != nil || !report.Complete() {
		t.Fatalf("erase normalized activity: %+v %v", report, err)
	}
	if from, known, err := store.PopulationCoverage(ctx, source.Tenant(), "gallery", signal.ActivityScope{Domain: "main", Language: "en"}, window); err != nil || !known || !from.Equal(start) {
		t.Fatalf("erasure moved instrumentation coverage: %v %v %v", from, known, err)
	}
}
