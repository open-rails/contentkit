package discovery

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
	"strings"
	"time"
)

// popularOrder reads only the configured bucket's stored snapshot. Cold buckets
// are explicitly pending; unrelated reaction/favorite scores never stand in.
func (s *Service) popularOrder(ctx context.Context, kind, id string, reader Reader, raw string) (order, *PopularitySnapshot, error) {
	key, ok := s.rankingFor(ctx, reader, kind, id)
	if !ok {
		return order{}, nil, fmt.Errorf("%w: popularity bucket %q is not configured for kind %q and this scope", ErrInvalid, id, kind)
	}
	snapshot, err := s.rankingSnapshot(ctx, key)
	if err != nil {
		return order{}, nil, err
	}
	if snapshot.AvailableFrom != nil && snapshot.WindowUntil != nil && !snapshot.WindowUntil.After(*snapshot.AvailableFrom) {
		return order{}, nil, &popularityRangeError{From: snapshot.AvailableFrom}
	}
	join, args := s.rankingJoin(key)
	return order{raw: raw, expr: "coalesce(activity.score,0)", join: join, desc: true, cast: "float8", args: args}, snapshot, nil
}

func popularSort(raw string) (string, bool) {
	name, id, _ := strings.Cut(raw, ":")
	return id, name == SortPopular
}

// rangeOrder ranks only prepared daily counts. It neither admits a cache row
// nor asks the worker to reconstruct historical activity.
func (s *Service) rangeOrder(ctx context.Context, kind string, window signal.Window, reader Reader, raw string) (order, *PopularitySnapshot, error) {
	key, ok := s.rankingFor(ctx, reader, kind, "")
	if !ok {
		return order{}, nil, fmt.Errorf("%w: popularity is not configured for this kind and scope", ErrInvalid)
	}
	scope := signal.ActivityScope{Domain: key.domain, Language: key.language}
	coverage, known, err := s.o.Hub.DailyPopularityCoverage(ctx, kind, scope)
	if err != nil {
		return order{}, nil, err
	}
	if !known {
		return order{}, nil, fmt.Errorf("%w: daily popularity has not been prepared", ErrInvalid)
	}
	if window.From.Before(coverage.From) || window.To.After(coverage.To) {
		return order{}, nil, &popularityRangeError{From: &coverage.From, Until: &coverage.To}
	}
	ids, err := popularity.Rank(ctx, s.o.Hub, s, kind, window, scope, PopularityDepth)
	if err != nil {
		return order{}, nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	args := pgx.NamedArgs{"d_range_ids": ids}
	join := `LEFT JOIN (SELECT r.content_id,2-r.ord::float8/(cardinality(@d_range_ids::text[])+1) AS score FROM unnest(@d_range_ids::text[]) WITH ORDINALITY AS r(content_id,ord)) activity ON activity.content_id=f.content_id `
	snapshot := &PopularitySnapshot{Ready: true, WindowFrom: &window.From, WindowUntil: &window.To, From: &window.From, Until: &window.To, AvailableFrom: &coverage.From}
	snapshot.Provisional = window.To.Add(24 * time.Hour).After(s.now())
	return order{raw: raw, expr: "coalesce(activity.score,0)", join: join, desc: true, cast: "float8", args: args}, snapshot, nil
}

type popularityRangeError struct{ From, Until *time.Time }

func (e *popularityRangeError) Error() string {
	message := "popularity range is outside prepared activity"
	if e.From != nil {
		message += "; available_from=" + e.From.UTC().Format(time.DateOnly)
	}
	if e.Until != nil {
		message += "; available_until=" + e.Until.UTC().Format(time.DateOnly) + " (exclusive)"
	}
	return message
}
func (e *popularityRangeError) Unwrap() error { return ErrInvalid }

func activityWindow(o ListOptions) (signal.Window, bool, error) {
	if o.ActivityFrom == "" && o.ActivityUntil == "" {
		return signal.Window{}, false, nil
	}
	invalid := func(message string) (signal.Window, bool, error) {
		return signal.Window{}, false, fmt.Errorf("%w: %s", ErrInvalid, message)
	}
	if o.ActivityFrom == "" || o.ActivityUntil == "" {
		return invalid("activity_from and activity_until are required together")
	}
	if o.Sort != SortPopular || o.Kind == "" {
		return invalid("activity dates require kind and sort=popular, without a preset period")
	}
	from, err := time.Parse(time.DateOnly, o.ActivityFrom)
	if err != nil || from.IsZero() {
		return invalid("activity_from must be a UTC date YYYY-MM-DD")
	}
	until, err := time.Parse(time.DateOnly, o.ActivityUntil)
	if err != nil || until.IsZero() {
		return invalid("activity_until must be a UTC date YYYY-MM-DD")
	}
	w := signal.Between(from, until)
	if err := w.Validate(); err != nil {
		return invalid("activity_until must be after activity_from")
	}
	return w, true, nil
}

func historicalSort(kind string, w signal.Window) string {
	return SortPopular + ":" + kind + ":" + w.String()
}
