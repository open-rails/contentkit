package popularity

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/signal"
)

// AvailabilityProvider supplies immutable publication and access history for a
// common covered portion of a requested activity window.
type AvailabilityProvider interface {
	Availability(context.Context, string, signal.Window, signal.ActivityScope) (signal.Availability, error)
}

type populationCoverage interface {
	PopulationCoverage(context.Context, string, signal.ActivityScope, signal.Window) (time.Time, bool, error)
}

// RankingInfo names the actual policy and common activity window, including an
// explicit unavailable result when no trusted audience history exists.
type RankingInfo struct {
	Policy         string `json:"policy"`
	Mode           string `json:"mode"`
	RequestedFrom  string `json:"requested_from,omitempty"`
	RequestedUntil string `json:"requested_until,omitempty"`
	CoveredFrom    string `json:"covered_from,omitempty"`
	CoveredUntil   string `json:"covered_until,omitempty"`
}

// PopularResult includes provenance even when the ranking is empty.
type PopularResult struct {
	Hits    []Hit       `json:"hits"`
	Ranking RankingInfo `json:"ranking"`
}

func activityDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// WithScope returns a request-scoped copy sharing only the bounded global cache.
func (r *Ranker) WithScope(scope signal.ActivityScope) *Ranker {
	copy := *r
	copy.scope = scope
	return &copy
}

func (r *Ranker) query(ctx context.Context, kind string, window signal.Window) (*Ranker, signal.Window, RankingInfo, error) {
	info := RankingInfo{Policy: r.policy.Name, Mode: "viewer_reach", RequestedFrom: activityDate(window.From), RequestedUntil: activityDate(window.To),
		CoveredFrom: activityDate(window.From), CoveredUntil: activityDate(window.To)}
	if !r.policy.Normalized() {
		return r, window, info, nil
	}
	copy := *r
	unavailable := func() (*Ranker, signal.Window, RankingInfo, error) {
		// Missing audience lineage cannot safely become unnormalized reach.
		// The host's catalog may supply a current explicit-feedback fallback.
		copy.standingUnavailable = true
		info.Policy, info.Mode = "none", "standing_unavailable"
		return &copy, window, info, nil
	}
	if r.availability == nil {
		return unavailable()
	}
	a, err := r.availability.Availability(ctx, kind, window, r.scope)
	if err != nil {
		return nil, window, info, err
	}
	a.Intervals = slices.Clone(a.Intervals)
	if provider, ok := r.source.(populationCoverage); ok && a.Known {
		from, known, err := provider.PopulationCoverage(ctx, kind, r.scope, window)
		if err != nil {
			return nil, window, info, err
		}
		a.Known = known
		if known && from.After(a.From) {
			a.From = from
			for i := range a.Intervals {
				if a.Intervals[i].From.Before(from) {
					a.Intervals[i].From = from
				}
			}
		}
	}
	if !a.Known {
		return unavailable()
	}
	// Audience intervals may be sub-day; the canonical signal window stays
	// whole-day while normalization clips every numerator/denominator to a.
	p := r.policy.Priors
	copy.normalization = &signal.NormalizationOptions{Availability: a, Scope: r.scope, PriorWeight: 20,
		Quality: signal.NormalizationPriors{Engagement: p.Engagement, EngagementWeight: p.EngagementWeight,
			Completion: p.Completion, CompletionWeight: p.CompletionWeight, Approval: p.Approval, ApprovalWeight: p.ApprovalWeight, RevisitWeight: p.RevisitWeight}}
	info.Mode, info.CoveredFrom, info.CoveredUntil = "population_normalized", activityDate(a.From), activityDate(a.Until)
	return &copy, window, info, nil
}

// PopularResult returns ranked works with the actual policy and coverage.
func (r *Ranker) PopularResult(ctx context.Context, kind string, window signal.Window, limit int) (PopularResult, error) {
	if strings.TrimSpace(kind) == "" {
		return PopularResult{Hits: []Hit{}}, fmt.Errorf("popularity: contentKind is required")
	}
	if err := window.Validate(); err != nil {
		return PopularResult{Hits: []Hit{}}, err
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, r.maxDepth)
	depth := r.readDepth(limit)
	key := r.cacheKey("popular_result", kind, window.String(), fmt.Sprint(depth))
	var result PopularResult
	if r.policy.Normalized() && r.cached(ctx, key, &result) {
		if len(result.Hits) > limit {
			result.Hits = result.Hits[:limit]
		}
		return result, nil
	}
	q, window, info, err := r.query(ctx, kind, window)
	if err != nil {
		return PopularResult{Hits: []Hit{}, Ranking: info}, err
	}
	read := limit
	if r.policy.Normalized() {
		q.cache = nil // The result and its coverage are cached atomically above.
		read = depth
	}
	hits, err := q.popular(ctx, kind, window, read)
	result = PopularResult{Hits: hits, Ranking: info}
	if err == nil && r.policy.Normalized() {
		r.store(ctx, key, result)
	}
	if len(result.Hits) > limit {
		result.Hits = result.Hits[:limit]
	}
	return result, err
}
