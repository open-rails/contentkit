package popularity_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

func TestIntegrationViewerWeightedQuality(t *testing.T) {
	ctx := context.Background()
	env := signaltest.FromEnv(t)
	const db = "contentkit_viewer_weight_review"
	conn := env.Fresh(t, db)
	t.Cleanup(func() { env.Drop(t, env.Open(t, ""), db) })
	hub := newHub(t, conn, db, fxTenant, nil)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	window := signal.Between(start, start.AddDate(0, 0, 30))
	base, heavy, heavyLate, audience, feedbackOnly := cid(201), cid(202), cid(203), cid(204), cid(205)
	var events []engagementtest.Observation
	add := func(work string, viewer, visit, day int) {
		events = append(events, engagementtest.Observation{ContentRef: ref(work), Subject: signal.Subject{UserID: fmt.Sprintf("u%d", viewer)},
			Type: engagementtest.TypeReading, EventID: fmt.Sprintf("s%d", visit), OccurredAt: start.AddDate(0, 0, day),
			Progress: 1, ProgressMax: 1, DurationS: 1})
	}
	for _, work := range []string{base, heavy, heavyLate, audience} {
		shift := 0
		if work == heavyLate {
			shift = 20
		}
		for viewer := 0; viewer < 100; viewer++ {
			add(work, viewer, 0, 1+shift)
		}
		if work == heavy || work == heavyLate {
			for visit := 1; visit < 1000; visit++ {
				add(work, 0, visit, 2+shift)
			}
		}
		if work == audience {
			for viewer := 0; viewer < 100; viewer++ {
				add(work, viewer, 1, 2)
			}
		}
	}
	// Explicit feedback alone supplies neither measured attention nor reach.
	events = append(events, engagementtest.Observation{ContentRef: ref(feedbackOnly), Subject: signal.Subject{UserID: "voter-only"},
		Type: "reaction", EventID: "current", OccurredAt: start.AddDate(0, 0, 1), Value: 1})
	recordBatched(t, hub, events)
	metrics := metricsByID(t, hub, []string{base, heavy, heavyLate, audience, feedbackOnly}, window)
	for id, want := range map[string]struct {
		returning uint64
		quality   float64
		activeMS  uint64
	}{
		base: {0, 12.5, 100_000}, heavy: {1, 13.375, 1_099_000}, heavyLate: {1, 13.375, 1_099_000}, audience: {100, 25, 200_000},
	} {
		m := metrics[id]
		if m.Readers != 100 || m.ReturningReaders != want.returning || math.Abs(m.ReaderQualitySum-want.quality) > 1e-12 || m.ActiveMS != want.activeMS {
			t.Fatalf("%s metrics: %+v, want %+v", id, m, want)
		}
	}
	if m := metrics[feedbackOnly]; m.Readers != 0 || m.ReaderQualitySum != 0 || m.ReturningReaders != 0 || m.ActiveMS != 0 {
		t.Fatalf("feedback-only activity became measured consumption: %+v", m)
	}
	p := popularity.PolicyV1
	bound := math.Log10(101) * (p.Weights.Engagement/(100+p.Priors.EngagementWeight) + p.Weights.Revisit/(100+p.Priors.RevisitWeight))
	if delta := p.Score(metrics[heavy]) - p.Score(metrics[base]); delta <= 0 || delta > bound {
		t.Fatalf("one repeater shifted score %g beyond bound %g", delta, bound)
	}
	if p.Score(metrics[audience]) <= p.Score(metrics[heavy]) {
		t.Fatal("audience-wide repeat use must outweigh one heavy repeater")
	}
	// Global SQL ranking and Go candidate ranking use the same read-derived
	// metrics and give identical time weight throughout each selected window.
	ranker, err := popularity.New(popularity.Config{Source: hub, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	global, err := ranker.Popular(ctx, fxKind, window, 10)
	if err != nil {
		t.Fatal(err)
	}
	scores := scoresByID(global)
	for id, m := range metrics {
		if math.Abs(scores[id]-p.Score(m)) > 1e-12 {
			t.Fatalf("%s SQL=%g Go=%g", id, scores[id], p.Score(m))
		}
	}
	if scores[heavy] != scores[heavyLate] {
		t.Fatalf("time decay leaked: %v", scores)
	}
	// The first day alone includes only initial one-second page exposures, not future repeats.
	first := metricsByID(t, hub, []string{heavy, audience}, signal.Between(start, start.AddDate(0, 0, 2)))
	for id, m := range first {
		if m.Readers != 100 || m.ActiveMS != 100_000 || m.ReturningReaders != 0 || m.ReaderQualitySum != 12.5 {
			t.Fatalf("%s first-day metrics: %+v", id, m)
		}
	}
}
