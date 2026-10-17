package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/gate"
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

// historicalOrder reuses the same bounded, policy-keyed ranking cache as the
// popularity package. Only window activity can enter this list; publication
// age, visibility and all reader filters remain in the ordinary page query.
// Measured readership ranks first; works with explicit standing in the window
// but no measured readers follow, so recent coverage or sparse reading never
// empties the list. Without a signal plane or audience lineage it is the
// labeled standing order, never a warm measured cache or unnormalized reach.
func (s *Service) historicalOrder(ctx context.Context, kind string, w signal.Window, reader Reader) (order, error) {
	o := order{raw: historicalSort(kind, w), expr: "activity.score", desc: true, cast: "float8", args: pgx.NamedArgs{}}
	if err := s.signals(); err != nil {
		if errors.Is(err, ErrSignalsDown) {
			s.o.Logger.WarnContext(ctx, "discovery: signal plane down; popular lists use standing", "kind", kind)
		}
		return s.standingOrder(o.raw, w, true), nil
	}
	host := reader.Host
	if host == "" {
		host = gate.Domain(ctx)
	}
	domain, _ := s.o.Kinds.DomainFor(host)
	result, err := s.historical.WithScope(signal.ActivityScope{Domain: domain.Name, Language: reader.Language}).PopularResult(ctx, kind, w, PopularityDepth)
	if err != nil {
		return o, err
	}
	if result.Ranking.Mode == "standing_unavailable" {
		return s.standingOrder(o.raw, w, true), nil
	}
	ranking := result.Ranking
	ranking.Mode = "measured_then_standing"
	o.ranking = &ranking
	o.rankings = map[string]ContentRanking{}
	ids, ranks := make([]string, 0, len(result.Hits)), make([]float64, 0, len(result.Hits))
	for _, h := range result.Hits {
		if h.Score > 0 {
			ids = append(ids, h.ContentID)
			if h.Normalization != nil {
				o.rankings[h.ContentID] = ContentRanking{Score: h.Score, Participation: h.Normalization.RawRate, AdjustedParticipation: h.Normalization.Rate}
			}
		}
	}
	// Hits arrive best first. Measured works score in (1, 2] by rank, above any
	// Wilson lower bound in [0, 1).
	for i := range ids {
		ranks = append(ranks, 1+float64(len(ids)-i)/float64(len(ids)))
	}
	standing := s.standingOrder(o.raw, w, true)
	o.join = "LEFT JOIN unnest(@d_activity_ids::text[], @d_activity_scores::float8[]) AS activity(content_id, score) ON activity.content_id = f.content_id " + standing.join
	o.expr = "COALESCE(activity.score, " + standing.expr + ")"
	o.args["d_activity_ids"], o.args["d_activity_scores"] = ids, ranks
	for k, v := range standing.args {
		o.args[k] = v
	}
	return o, nil
}
