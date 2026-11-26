package discoverystore

import (
	"context"
	"fmt"
	"strings"
	"time"

	discovery "github.com/open-rails/contentkit/discovery"
)

// popularOrder reads only the configured bucket's stored snapshot. Cold buckets
// are explicitly pending; unrelated reaction/favorite scores never stand in.
func (s *Service) popularOrder(ctx context.Context, kind, id string, reader discovery.Reader, raw string) (order, *discovery.PopularitySnapshot, error) {
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
	return id, name == discovery.SortPopular
}

// rangeOrder ranked prepared daily activity, which is gone with ClickHouse's
// daily popularity tables; a custom range has no source until rankings do.
func (s *Service) rangeOrder(context.Context, string, dayWindow, discovery.Reader, string) (order, *discovery.PopularitySnapshot, error) {
	return order{}, nil, fmt.Errorf("%w: popularity over a custom range is unavailable", ErrInvalid)
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

// dayWindow is a range of whole UTC days, [from, until).
type dayWindow struct{ from, until time.Time }

func activityWindow(o ListOptions) (dayWindow, bool, error) {
	if o.ActivityFrom == "" && o.ActivityUntil == "" {
		return dayWindow{}, false, nil
	}
	invalid := func(message string) (dayWindow, bool, error) {
		return dayWindow{}, false, fmt.Errorf("%w: %s", ErrInvalid, message)
	}
	if o.ActivityFrom == "" || o.ActivityUntil == "" {
		return invalid("activity_from and activity_until are required together")
	}
	if o.Sort != discovery.SortPopular || o.Kind == "" {
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
	if !from.Before(until) {
		return invalid("activity_until must be after activity_from")
	}
	return dayWindow{from, until}, true, nil
}

func historicalSort(kind string, w dayWindow) string {
	return discovery.SortPopular + ":" + kind + ":[" + w.from.Format(time.DateOnly) + "," + w.until.Format(time.DateOnly) + ")"
}
