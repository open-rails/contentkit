package popularity

import (
	"context"
	"slices"
	"time"

	"github.com/open-rails/contentkit/signal"
)

// Source is one tenant's signal plane; the engine's hub satisfies it.
type Source interface {
	PrepareDailyPopularity(ctx context.Context, kind string, availability signal.Availability, scope signal.ActivityScope, rebuild bool) (signal.DailyPopularityPreparation, error)
	DailyPopular(ctx context.Context, kind string, window signal.Window, scope signal.ActivityScope, limit int) ([]signal.PopularHit, error)
	// PopulationCoverage is when trusted audience observations start for scope.
	PopulationCoverage(ctx context.Context, kind string, scope signal.ActivityScope, window signal.Window) (time.Time, bool, error)
}

// AvailabilityProvider supplies immutable publication and access history for a
// common covered portion of a requested activity window, for the given
// contents of the kind.
type AvailabilityProvider interface {
	Availability(ctx context.Context, kind string, window signal.Window, scope signal.ActivityScope, contents []string) (signal.Availability, error)
}

// Prepare is the existing worker's once-per-kind/scope preparation step for
// all due bucket windows. It shares daily source work across overlapping ranges.
func Prepare(ctx context.Context, source Source, availability AvailabilityProvider, kind string, windows []signal.Window, scope signal.ActivityScope) error {
	return prepare(ctx, source, availability, kind, windows, scope, false)
}

// Rebuild explicitly recalculates closed historical days after an import or
// correction. It reports failures and creates no scheduled repair work.
func Rebuild(ctx context.Context, source Source, availability AvailabilityProvider, kind string, windows []signal.Window, scope signal.ActivityScope) error {
	return prepare(ctx, source, availability, kind, windows, scope, true)
}

func prepare(ctx context.Context, source Source, availability AvailabilityProvider, kind string, windows []signal.Window, scope signal.ActivityScope, rebuild bool) error {
	windows = slices.Clone(windows)
	for _, window := range windows {
		if err := window.Validate(); err != nil {
			return err
		}
	}
	slices.SortFunc(windows, func(a, b signal.Window) int { return a.From.Compare(b.From) })
	var combined []signal.Window
	for _, window := range windows {
		if len(combined) == 0 {
			combined = append(combined, window)
			continue
		}
		last := &combined[len(combined)-1]
		if last.To.IsZero() || window.From.IsZero() || !window.From.After(last.To) {
			if last.To.IsZero() || window.To.IsZero() {
				last.To = time.Time{}
			} else if window.To.After(last.To) {
				last.To = window.To
			}
			continue
		}
		combined = append(combined, window)
	}
	for _, window := range combined {
		a, err := availability.Availability(ctx, kind, window, scope, nil)
		if err != nil {
			return err
		}
		if !a.Known {
			continue
		}
		from, known, err := source.PopulationCoverage(ctx, kind, scope, window)
		if err != nil {
			return err
		}
		if !known {
			continue
		}
		if from.After(a.From) {
			a.From = from
			a.Intervals = slices.Clone(a.Intervals)
			for i := range a.Intervals {
				if a.Intervals[i].From.Before(from) {
					a.Intervals[i].From = from
				}
			}
		}
		if _, err := source.PrepareDailyPopularity(ctx, kind, a, scope, rebuild); err != nil {
			return err
		}
	}
	return nil
}

// Rank reads only the prepared daily scalars. Prepare must run once for the
// scope before its bucket reads; public requests never invoke preparation.
func Rank(ctx context.Context, source Source, _ AvailabilityProvider, kind string, window signal.Window, scope signal.ActivityScope, limit int) ([]string, error) {
	hits, err := source.DailyPopular(ctx, kind, window, scope, limit)
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
