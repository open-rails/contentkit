// Package popularity ranks host content by a named, bounded policy over the
// signal plane's canonical window metrics (docs/popularity-policy.md).
//
//	reach   = log10(1 + viewers)
//	engage  = (mean_session_score · viewers + e0·ke) / (viewers + ke)
//	finish  = (completers + f0·kf) / (viewers + kf)
//	approve = (positive + a0·ka) / (positive + negative + ka)
//	revisit = min(1, (views − viewers) / (viewers + kr))
//	rank    = reach × (1 + we·engage + wf·finish + wa·approve + wr·revisit)
//
// Every quality term lies in [0, 1], so reach (distinct subjects) dominates
// and the multiplier is bounded by MaxQuality. Sessions set the engagement
// mean, viewers its confidence: one subject's repeated sessions, checkpoints
// and retries are one observation. Priors are pseudo-observations: one vote
// moves approve by at most 1/(ka+1). Time only selects the window; no term
// depends on when an observation happened inside it. The same formula runs in
// ClickHouse (RankExpr, global top-N) and in Go (Score, candidate sets), and
// public counts are the raw metrics, never derived from a rank.
package popularity

import (
	"fmt"
	"math"
	"sort"

	"github.com/open-rails/contentkit/signal"
)

// Weights are the quality term weights, each in [0, 1].
type Weights struct {
	Engagement float64
	Completion float64
	Approval   float64
	Revisit    float64
}

// Priors smooth each term with pseudo-observations: a prior mean in [0, 1]
// and its weight (>= 1) in pseudo-viewers or pseudo-votes.
type Priors struct {
	Engagement       float64
	EngagementWeight float64
	Completion       float64
	CompletionWeight float64
	Approval         float64
	ApprovalWeight   float64
	RevisitWeight    float64
}

// Policy is one named, immutable ranking. Changing a weight or prior is a new
// name, never an edit of an existing one: configuration and cache keys carry
// the name.
type Policy struct {
	Name    string
	Weights Weights
	Priors  Priors
}

// PolicyV1 is the qualified policy: weights and priors selected by the judged
// fixture recorded in docs/popularity-policy.md.
var PolicyV1 = Policy{
	Name:    "v1",
	Weights: Weights{Engagement: 0.35, Completion: 0.25, Approval: 0.40, Revisit: 0.10},
	Priors: Priors{
		Engagement: 0.40, EngagementWeight: 10,
		Completion: 0.30, CompletionWeight: 10,
		Approval: 0.75, ApprovalWeight: 5,
		RevisitWeight: 10,
	},
}

// DefaultName is the policy a host gets when it configures none.
const DefaultName = "v1"

var policies = map[string]Policy{PolicyV1.Name: PolicyV1}

// ByName resolves a configured policy. Unknown names are errors so a host
// refuses to start rather than ranking under a silent default.
func ByName(name string) (Policy, error) {
	if name == "" {
		name = DefaultName
	}
	p, ok := policies[name]
	if !ok {
		return Policy{}, fmt.Errorf("popularity: unknown policy %q (known: %v)", name, Names())
	}
	return p, nil
}

// Names lists the registered policies.
func Names() []string {
	names := make([]string, 0, len(policies))
	for n := range policies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Validate rejects parameters outside the documented bounds.
func (p Policy) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("popularity: policy needs a name")
	}
	for _, w := range []struct {
		name string
		v    float64
	}{{"engagement weight", p.Weights.Engagement}, {"completion weight", p.Weights.Completion}, {"approval weight", p.Weights.Approval}, {"revisit weight", p.Weights.Revisit}, {"engagement prior", p.Priors.Engagement}, {"completion prior", p.Priors.Completion}, {"approval prior", p.Priors.Approval}} {
		if w.v < 0 || w.v > 1 || math.IsNaN(w.v) {
			return fmt.Errorf("popularity: policy %s: %s %g outside [0,1]", p.Name, w.name, w.v)
		}
	}
	for _, k := range []struct {
		name string
		v    float64
	}{{"engagement prior weight", p.Priors.EngagementWeight}, {"completion prior weight", p.Priors.CompletionWeight}, {"approval prior weight", p.Priors.ApprovalWeight}, {"revisit prior weight", p.Priors.RevisitWeight}} {
		if !(k.v >= 1) || math.IsInf(k.v, 0) {
			return fmt.Errorf("popularity: policy %s: %s %g must be >= 1", p.Name, k.name, k.v)
		}
	}
	return nil
}

// MaxQuality is the largest multiplier the quality terms can reach.
func (p Policy) MaxQuality() float64 {
	return 1 + p.Weights.Engagement + p.Weights.Completion + p.Weights.Approval + p.Weights.Revisit
}

// Score ranks one work's window metrics. Works without a view score 0.
func (p Policy) Score(m signal.ContentMetrics) float64 {
	viewers := float64(m.Viewers)
	views := float64(m.Views)
	if views == 0 || viewers == 0 {
		return 0
	}
	meanScore := math.Max(0, math.Min(1, float64(m.ScoreSum)/(100*views)))
	engage := (meanScore*viewers + p.Priors.Engagement*p.Priors.EngagementWeight) / (viewers + p.Priors.EngagementWeight)
	finish := (float64(m.Completers) + p.Priors.Completion*p.Priors.CompletionWeight) / (viewers + p.Priors.CompletionWeight)
	positive, negative := float64(m.PositiveSubjects), float64(m.NegativeSubjects)
	approve := (positive + p.Priors.Approval*p.Priors.ApprovalWeight) / (positive + negative + p.Priors.ApprovalWeight)
	revisit := math.Min(1, (views-viewers)/(viewers+p.Priors.RevisitWeight))
	quality := 1 + p.Weights.Engagement*engage + p.Weights.Completion*finish + p.Weights.Approval*approve + p.Weights.Revisit*revisit
	return math.Log10(1+viewers) * quality
}

// RankExpr renders Score as a ClickHouse expression over the window metric
// columns for signal.PopularOptions.RankExpr. Parameters are rendered as
// literals; nothing user-controlled reaches it.
func (p Policy) RankExpr() string {
	return fmt.Sprintf(`log10(1 + toFloat64(viewers)) * (1
 + %s * ((greatest(0, least(1, toFloat64(score_sum) / (100 * toFloat64(views)))) * toFloat64(viewers) + %s) / (toFloat64(viewers) + %s))
 + %s * ((toFloat64(completers) + %s) / (toFloat64(viewers) + %s))
 + %s * ((toFloat64(positive_subjects) + %s) / (toFloat64(positive_subjects) + toFloat64(negative_subjects) + %s))
 + %s * least(1, (toFloat64(views) - toFloat64(viewers)) / (toFloat64(viewers) + %s)))`,
		lit(p.Weights.Engagement), lit(p.Priors.Engagement*p.Priors.EngagementWeight), lit(p.Priors.EngagementWeight),
		lit(p.Weights.Completion), lit(p.Priors.Completion*p.Priors.CompletionWeight), lit(p.Priors.CompletionWeight),
		lit(p.Weights.Approval), lit(p.Priors.Approval*p.Priors.ApprovalWeight), lit(p.Priors.ApprovalWeight),
		lit(p.Weights.Revisit), lit(p.Priors.RevisitWeight))
}

// lit renders a float as a ClickHouse Float64 literal with full precision.
func lit(v float64) string {
	return fmt.Sprintf("toFloat64(%s)", fmtFloat(v))
}

func fmtFloat(v float64) string {
	s := fmt.Sprintf("%.17g", v)
	for _, c := range s {
		if c == '.' || c == 'e' || c == 'n' || c == 'i' {
			return s
		}
	}
	return s + ".0"
}
