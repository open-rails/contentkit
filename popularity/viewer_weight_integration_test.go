package popularity_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

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
	base, heavy, heavyLate, audience, legacy := cid(201), cid(202), cid(203), cid(204), cid(205)
	var events []signal.Signal
	add := func(work string, viewer, session int, score int16, day int) {
		events = append(events, signal.Signal{ContentRef: ref(work), Subject: signal.Subject{UserID: fmt.Sprintf("u%d", viewer)}, Type: signal.TypeView, EventID: fmt.Sprintf("s%d", session), OccurredAt: start.AddDate(0, 0, day), Score: score})
	}
	for _, work := range []string{base, heavy, heavyLate, audience} {
		shift := 0
		if work == heavyLate {
			shift = 20
		}
		for viewer := 0; viewer < 100; viewer++ {
			add(work, viewer, 0, 0, 1+shift)
		}
		if work == heavy || work == heavyLate {
			for session := 1; session < 1000; session++ {
				add(work, 0, session, 100, 2+shift)
			}
		}
		if work == audience {
			for viewer := 0; viewer < 100; viewer++ {
				add(work, viewer, 1, 100, 2)
			}
		}
	}
	// Legacy negative means clamp per subject; feedback-only subjects do not
	// contribute an engagement observation or divide by zero.
	add(legacy, 0, 0, -30, 1)
	events = append(events, signal.Signal{ContentRef: ref(legacy), Subject: signal.Subject{UserID: "voter-only"}, Type: "reaction", EventID: "current", OccurredAt: start.AddDate(0, 0, 1), Value: 1})
	recordBatched(t, hub, events)
	metrics := metricsByID(t, hub, []string{base, heavy, heavyLate, audience, legacy}, window)
	for id, want := range map[string]struct {
		views, returning uint64
		engagement       float64
		score            int64
	}{
		base: {100, 0, 0, 0}, heavy: {1099, 1, .999, 99900}, heavyLate: {1099, 1, .999, 99900}, audience: {200, 100, 50, 10000},
	} {
		m := metrics[id]
		if m.Viewers != 100 || m.Views != want.views || m.ReturningViewers != want.returning || math.Abs(m.ViewerEngagementSum-want.engagement) > 1e-12 || m.ScoreSum != want.score {
			t.Fatalf("%s metrics: %+v, want %+v", id, m, want)
		}
	}
	if m := metrics[legacy]; m.Viewers != 1 || m.ViewerEngagementSum != 0 || m.ReturningViewers != 0 || m.ScoreSum != -30 {
		t.Fatalf("legacy/feedback-only metric: %+v", m)
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
	// The first day alone includes only initial sessions, not future repeats.
	first := metricsByID(t, hub, []string{heavy, audience}, signal.Between(start, start.AddDate(0, 0, 2)))
	for id, m := range first {
		if m.Viewers != 100 || m.Views != 100 || m.ReturningViewers != 0 || m.ViewerEngagementSum != 0 {
			t.Fatalf("%s first-day metrics: %+v", id, m)
		}
	}
}
