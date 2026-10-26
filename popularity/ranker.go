package popularity

import (
	"context"
	"slices"
	"time"

	"github.com/open-rails/contentkit/signal"
)

// Source is one tenant's signal plane; the engine's hub satisfies it.
type Source interface {
	Popular(ctx context.Context, contentKind string, opts signal.PopularOptions) ([]signal.PopularHit, error)
	// ReadContent lists the works of kind read in window.
	ReadContent(ctx context.Context, kind string, window signal.Window) ([]string, error)
	// PopulationCoverage is when trusted audience observations start for scope.
	PopulationCoverage(ctx context.Context, kind string, scope signal.ActivityScope, window signal.Window) (time.Time, bool, error)
}

// AvailabilityProvider supplies immutable publication and access history for a
// common covered portion of a requested activity window, for the given
// contents of the kind.
type AvailabilityProvider interface {
	Availability(ctx context.Context, kind string, window signal.Window, scope signal.ActivityScope, contents []string) (signal.Availability, error)
}

// Rank returns up to limit works of kind with qualified readers in window,
// best first. Without trusted audience history it returns none: missing
// lineage never becomes unnormalized reach.
func Rank(ctx context.Context, source Source, availability AvailabilityProvider, kind string, window signal.Window, scope signal.ActivityScope, limit int) ([]string, error) {
	contents, err := source.ReadContent(ctx, kind, window)
	if err != nil {
		return nil, err
	}
	a, err := availability.Availability(ctx, kind, window, scope, contents)
	if err != nil || !a.Known {
		return nil, err
	}
	a.Intervals = slices.Clone(a.Intervals)
	from, known, err := source.PopulationCoverage(ctx, kind, scope, window)
	if err != nil || !known {
		return nil, err
	}
	if from.After(a.From) {
		a.From = from
		for i := range a.Intervals {
			if a.Intervals[i].From.Before(from) {
				a.Intervals[i].From = from
			}
		}
	}
	// Audience intervals may be sub-day; the canonical signal window stays
	// whole-day while normalization clips every numerator/denominator to a.
	p := PolicyV2.Priors
	hits, err := source.Popular(ctx, kind, signal.PopularOptions{Window: window, Limit: limit, RankExpr: PolicyV2.RankExpr(),
		Normalization: &signal.NormalizationOptions{Availability: a, Scope: scope, PriorWeight: 20,
			Quality: signal.NormalizationPriors{Engagement: p.Engagement, EngagementWeight: p.EngagementWeight,
				Completion: p.Completion, CompletionWeight: p.CompletionWeight, Approval: p.Approval, ApprovalWeight: p.ApprovalWeight, RevisitWeight: p.RevisitWeight}}})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.Score > 0 {
			ids = append(ids, h.ContentID)
		}
	}
	return ids, nil
}
