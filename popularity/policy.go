// Package popularity ranks works by their event-time eligible-population
// participation over immutable access intervals, weighted by bounded quality
// (docs/popularity-policy.md):
//
//	rank = rate + we·engage + wf·finish + wa·approve + wr·revisit
//
// where every term is the duration-weighted mean over a work's intervals of
// its smoothed participation rate times the interval's smoothed quality term.
package popularity

import "fmt"

// Weights are the quality term weights, each in [0, 1].
type Weights struct {
	Engagement float64
	Completion float64
	Approval   float64
	Revisit    float64
}

// Priors smooth each term with pseudo-observations: a prior mean in [0, 1]
// and its weight (>= 1) in pseudo-readers or pseudo-votes.
type Priors struct {
	Engagement       float64
	EngagementWeight float64
	Completion       float64
	CompletionWeight float64
	Approval         float64
	ApprovalWeight   float64
	RevisitWeight    float64
}

// Policy is the ranking's weights and priors.
type Policy struct {
	Weights Weights
	Priors  Priors
}

// PolicyV2 normalizes each historical access interval by its active eligible
// population. Its twenty pseudo-eligible nonreaders are a modest reliability
// heuristic, not a statistical confidence interval.
var PolicyV2 = Policy{
	Weights: Weights{Engagement: 0.35, Completion: 0.25, Approval: 0.40, Revisit: 0.10},
	Priors: Priors{
		Engagement: 0.40, EngagementWeight: 10,
		Completion: 0.30, CompletionWeight: 10,
		Approval: 0.75, ApprovalWeight: 5,
		RevisitWeight: 10,
	},
}

// RankExpr renders the policy as a ClickHouse expression over the normalized
// metric columns for signal.PopularOptions.RankExpr. Parameters are rendered
// as literals; nothing user-controlled reaches it.
func (p Policy) RankExpr() string {
	return fmt.Sprintf("normalized_rate + %s*normalized_engagement + %s*normalized_completion + %s*normalized_approval + %s*normalized_revisit",
		lit(p.Weights.Engagement), lit(p.Weights.Completion), lit(p.Weights.Approval), lit(p.Weights.Revisit))
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
