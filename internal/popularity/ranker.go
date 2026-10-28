package popularity

import (
	"context"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/open-rails/contentkit/signal"
)

// Source is one tenant's signal plane; the engine's hub satisfies it.
type Source interface {
	PrepareDailyPopularity(ctx context.Context, kind string, availability signal.Availability, scope signal.ActivityScope, rebuild bool) (signal.DailyPopularityPreparation, error)
	DailyPopular(ctx context.Context, kind string, window signal.Window, scope signal.ActivityScope, limit int) ([]signal.PopularHit, error)
	DailyPopularityCoverage(ctx context.Context, kind string, scope signal.ActivityScope) (signal.Window, bool, error)
	PopulationCoverage(ctx context.Context, kind string, scope signal.ActivityScope, window signal.Window) (time.Time, bool, error)
}

// AvailabilityProvider supplies immutable publication and access history.
type AvailabilityProvider interface {
	Availability(ctx context.Context, kind string, window signal.Window, scope signal.ActivityScope, contents []string) (signal.Availability, error)
}

// Prepare prepares every known day once per kind/scope in the existing worker.
// It retains closed days independently of configured preset windows. Until is
// the worker's single refresh-cycle time, not a reader-supplied query boundary.
// Returned coverage contains only successfully prepared UTC days.
func Prepare(ctx context.Context, source Source, availability AvailabilityProvider, kind string, until time.Time, scope signal.ActivityScope) (signal.Window, error) {
	return prepare(ctx, source, availability, kind, until, scope, false)
}

// Rebuild explicitly recalculates historical corrections/imports. It reports
// errors and creates no scheduled repair work.
func Rebuild(ctx context.Context, source Source, availability AvailabilityProvider, kind string, until time.Time, scope signal.ActivityScope) (signal.Window, error) {
	return prepare(ctx, source, availability, kind, until, scope, true)
}

func prepare(ctx context.Context, source Source, availability AvailabilityProvider, kind string, until time.Time, scope signal.ActivityScope, rebuild bool) (signal.Window, error) {
	a, err := availability.Availability(ctx, kind, signalstore.AllTime(), scope, nil)
	if err != nil {
		return signal.Window{}, err
	}
	if a.Known {
		from, known, err := source.PopulationCoverage(ctx, kind, scope, signalstore.AllTime())
		if err != nil {
			return signal.Window{}, err
		}
		if known {
			if from.After(a.From) {
				a.From = from
			}
			if until.Before(a.Until) {
				a.Until = until
			}
			if _, err := source.PrepareDailyPopularity(ctx, kind, a, scope, rebuild); err != nil {
				return signal.Window{}, err
			}
		}
	}
	covered, _, err := source.DailyPopularityCoverage(ctx, kind, scope)
	return covered, err
}

// Rank reads only the prepared daily scalars. Public requests may read an
// arbitrary whole-day range after checking DailyPopularityCoverage; they never
// invoke preparation or scan raw receipts.
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
