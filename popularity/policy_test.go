package popularity

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/signal"
)

func TestPolicyRegistry(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "v1"} {
		p, err := ByName(name)
		if err != nil || p.Name != "v1" || p != PolicyV1 {
			t.Fatalf("ByName(%q) = %+v, %v", name, p, err)
		}
	}
	if _, err := ByName("v0"); err == nil || !strings.Contains(err.Error(), `unknown policy "v0"`) {
		t.Fatalf("unknown policy must refuse: %v", err)
	}
	if names := Names(); len(names) != 1 || names[0] != "v1" {
		t.Fatalf("names %v", names)
	}
	if err := PolicyV1.Validate(); err != nil {
		t.Fatal(err)
	}
	if PolicyV1.MaxQuality() != 2.1 {
		t.Fatalf("max quality %g", PolicyV1.MaxQuality())
	}
}

func TestPolicyValidateBounds(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Name = "" },
		func(p *Policy) { p.Weights.Engagement = 1.5 },
		func(p *Policy) { p.Weights.Approval = -0.1 },
		func(p *Policy) { p.Priors.Approval = 1.2 },
		func(p *Policy) { p.Priors.CompletionWeight = 0 },
		func(p *Policy) { p.Priors.RevisitWeight = math.NaN() },
		func(p *Policy) { p.Priors.EngagementWeight = math.Inf(1) },
	} {
		p := PolicyV1
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%+v must be invalid", p)
		}
	}
	if _, err := New(Config{Source: fakeSource{}, Policy: Policy{Name: "bad", Priors: Priors{}}}); err == nil {
		t.Fatal("New must refuse an invalid policy")
	}
	if _, err := New(Config{Policy: PolicyV1}); err == nil {
		t.Fatal("New must require a Source")
	}
}

func TestPolicyScoreIsBoundedByReach(t *testing.T) {
	t.Parallel()
	p := PolicyV1
	for _, m := range []signal.ContentMetrics{
		{Viewers: 1, Views: 1},
		{Viewers: 50, Views: 500, Completers: 50, ScoreSum: 50000, ViewerEngagementSum: 50, ReturningViewers: 50, PositiveSubjects: 50},
		{Viewers: 1000, Views: 1000, NegativeSubjects: 1000},
		{Viewers: 3, Views: 3, ScoreSum: -90}, // legacy negative scores clamp
	} {
		score := p.Score(m)
		reach := math.Log10(1 + float64(m.Viewers))
		if score < reach || score > reach*p.MaxQuality() {
			t.Errorf("%+v: score %g outside [%g, %g]", m, score, reach, reach*p.MaxQuality())
		}
	}
	if p.Score(signal.ContentMetrics{PositiveSubjects: 9}) != 0 || p.Score(signal.ContentMetrics{}) != 0 {
		t.Fatal("feedback without a view never ranks")
	}
}

func TestPolicyOneVoteCannotDominate(t *testing.T) {
	t.Parallel()
	p := PolicyV1
	base := signal.ContentMetrics{Viewers: 20, Views: 20, Completers: 10, ScoreSum: 1600, ViewerEngagementSum: 16}
	liked, disliked := base, base
	liked.PositiveSubjects = 1
	disliked.NegativeSubjects = 1
	if !(p.Score(liked) > p.Score(base)) || !(p.Score(disliked) < p.Score(base)) {
		t.Fatal("a vote must move the score in its direction")
	}
	maxShift := p.Weights.Approval / (p.Priors.ApprovalWeight + 1)
	for _, m := range []signal.ContentMetrics{liked, disliked} {
		if shift := math.Abs(p.Score(m)-p.Score(base)) / math.Log10(21); shift >= maxShift {
			t.Errorf("one vote shifted quality by %g, bound %g", shift, maxShift)
		}
	}
	more := base
	more.Viewers, more.Views = 25, 25
	if !(p.Score(more) > p.Score(liked)) {
		t.Fatal("25 unvoted viewers beat 20 viewers plus one like")
	}
}

// The documented no_votes fixture gallery: 20 readers, 8/10 pages, 80 s.
func TestPolicyScoreMatchesDocumentedFormula(t *testing.T) {
	t.Parallel()
	m := signal.ContentMetrics{Viewers: 20, Views: 20, ScoreSum: 1600, ViewerEngagementSum: 16}
	engage := (0.8*20 + 0.40*10) / (20 + 10)
	finish := (0 + 0.30*10) / (20 + 10)
	approve := (0 + 0.75*5) / (0 + 0 + 5)
	want := math.Log10(21) * (1 + 0.35*engage + 0.25*finish + 0.40*approve)
	if got := PolicyV1.Score(m); math.Abs(got-want) > 1e-12 {
		t.Fatalf("score %.12f want %.12f", got, want)
	}
}

func TestPolicyRankExprRendersLiterals(t *testing.T) {
	t.Parallel()
	expr := PolicyV1.RankExpr()
	for _, col := range []string{"viewers", "completers", "viewer_engagement_sum", "returning_viewers", "positive_subjects", "negative_subjects"} {
		if !strings.Contains(expr, col) {
			t.Errorf("expression lacks %s", col)
		}
	}
	if strings.Contains(expr, "?") || strings.Count(expr, "log10") != 1 || !strings.Contains(expr, "toFloat64(10.0)") {
		t.Fatalf("expression: %s", expr)
	}
	if lit(0.35) != "toFloat64(0.34999999999999998)" || lit(5) != "toFloat64(5.0)" {
		t.Fatalf("literals %s %s", lit(0.35), lit(5))
	}
}

func TestWindowsAreLiteral(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 17, 15, 30, 0, 0, time.FixedZone("x", 5*3600))
	day := func(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	want := map[string]signal.Window{
		"7d":   {From: day(2026, 9, 11), To: day(2026, 9, 18)},
		"30d":  {From: day(2026, 8, 19), To: day(2026, 9, 18)},
		"90d":  {From: day(2026, 6, 20), To: day(2026, 9, 18)},
		"365d": {From: day(2025, 9, 18), To: day(2026, 9, 18)},
		"all":  {},
	}
	keys := map[string]bool{}
	for _, period := range Periods {
		w, err := WindowForPeriod(period, now)
		if err != nil || w != want[period] {
			t.Fatalf("%s: %v %v (want %v)", period, w, err, want[period])
		}
		keys[w.String()] = true
	}
	if len(keys) != len(Periods) || len(want) != len(Periods) {
		t.Fatalf("periods %v keys %v", Periods, keys)
	}
	for _, bad := range []string{"", "14d", "7", "week"} {
		if _, err := WindowForPeriod(bad, now); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if _, err := WindowForPeriod(DefaultPeriod, now); err != nil {
		t.Fatal(err)
	}
}

func TestSessionScoreIsCoverageTimesDwell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                    string
		covered, total, activeS uint32
		score                   int16
		completed               bool
	}{
		{"short read fully", 6, 6, 60, 100, true},
		{"long read fully", 200, 200, 2000, 100, true},
		{"long skimmed", 30, 200, 300, 15, false},
		{"eight of ten pages", 8, 10, 80, 80, false},
		{"nine of ten completes", 9, 10, 72, 90, true},
		{"half of twelve", 6, 12, 48, 50, false},
		{"flipped through", 10, 10, 0, 60, true},
		{"half dwell", 10, 10, 40, 80, true},
		{"nothing covered", 0, 10, 100, 0, false},
		{"no total", 5, 0, 100, 0, false},
	}
	for _, c := range cases {
		score, completed := SessionScore(c.covered, c.total, c.activeS, 8)
		if score != c.score || completed != c.completed {
			t.Errorf("%s: score %d completed %v, want %d %v", c.name, score, completed, c.score, c.completed)
		}
	}
	if score, _ := SessionScore(30, 60, 30, 0); score != 50 {
		t.Fatalf("seconds as units: dwell is 1, score %d", score)
	}

	scorer := SessionScorer{SecondsPerUnit: 8, CoveredKey: "unique_pages_viewed"}
	view := signal.Signal{Type: signal.TypeView, Progress: 10, ProgressMax: 10, DurationS: 30, Payload: map[string]any{"unique_pages_viewed": uint32(3)}}
	scored, err := scorer.Score(context.Background(), view)
	if err != nil || scored.Score != 30 || scored.Completed || scored.Progress != 10 || scored.ProgressMax != 10 {
		t.Fatalf("covered from payload, progress kept: %+v %v", scored, err)
	}
	view.Payload = nil
	if scored, _ = scorer.Score(context.Background(), view); scored.Score != 75 || !scored.Completed {
		t.Fatalf("covered from progress: %+v", scored)
	}
	reaction := signal.Signal{Type: "reaction", Value: 1, Score: 7, Completed: true}
	if scored, _ = scorer.Score(context.Background(), reaction); scored.Score != 7 || !scored.Completed {
		t.Fatalf("non-view signals pass through: %+v", scored)
	}
}

func TestMemoryCacheExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemoryCache()
	c.Set(ctx, "k", []byte("v"), time.Millisecond)
	c.Set(ctx, "forever", []byte("v"), 0)
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("expired entry served")
	}
	if v, ok := c.Get(ctx, "forever"); !ok || string(v) != "v" {
		t.Fatal("ttl 0 must not expire")
	}
}

type fakeSource struct{}

func (fakeSource) Tenant() string { return "t" }
func (fakeSource) Popular(context.Context, string, signal.PopularOptions) ([]signal.PopularHit, error) {
	return nil, nil
}
func (fakeSource) Metrics(context.Context, []signal.ContentRef, signal.Window) (map[signal.ContentKey]signal.ContentMetrics, error) {
	return nil, nil
}

// Session volume cannot turn one viewer into evidence about the whole audience.
func TestPolicyOneRepeaterHasOneViewerInfluence(t *testing.T) {
	p := PolicyV1
	baseline := signal.ContentMetrics{Viewers: 100, Views: 100}
	heavy := baseline
	heavy.Views, heavy.ScoreSum = 1099, 99900
	heavy.ViewerEngagementSum, heavy.ReturningViewers = .999, 1
	delta := p.Score(heavy) - p.Score(baseline)
	bound := math.Log10(101) * (p.Weights.Engagement/(100+p.Priors.EngagementWeight) + p.Weights.Revisit/(100+p.Priors.RevisitWeight))
	if delta <= 0 || delta > bound {
		t.Fatalf("one repeater shifted rank by %g; maximum one-viewer bound %g", delta, bound)
	}
	audience := baseline
	audience.Views, audience.ScoreSum = 200, 10000
	audience.ViewerEngagementSum, audience.ReturningViewers = 50, 100
	if p.Score(audience) <= p.Score(heavy) {
		t.Fatalf("audience-wide repeat use must outrank one heavy viewer: audience=%g heavy=%g", p.Score(audience), p.Score(heavy))
	}
}
