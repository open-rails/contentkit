package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/contentkit/signal"
)

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

// historicalOrder orders by the stored ranking of the kind, period and reader
// scope. Only window activity can enter this list; publication age,
// visibility and all reader filters remain in the ordinary page query.
// Ranked works come first; works with explicit standing in the window but no
// ranking follow, so recent coverage, sparse reading or a ranking not built
// yet never empties the list. Without a signal plane it is the standing order.
func (s *Service) historicalOrder(ctx context.Context, kind, period string, w signal.Window, reader Reader) (order, error) {
	o := s.standingOrder(historicalSort(kind, w), w, true)
	if err := s.signals(); err != nil {
		if errors.Is(err, ErrSignalsDown) {
			s.o.Logger.WarnContext(ctx, "discovery: signal plane down; popular lists use standing", "kind", kind)
		}
		return o, nil
	}
	key := s.rankingFor(ctx, reader, kind, period)
	if err := s.requestRanking(ctx, key); err != nil {
		return o, err
	}
	join, args := s.rankingJoin(key)
	o.join = join + o.join
	o.expr = "COALESCE(activity.score, " + o.expr + ")"
	for k, v := range args {
		o.args[k] = v
	}
	return o, nil
}
