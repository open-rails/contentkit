package discovery

import (
	"context"
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
func (s *Service) historicalOrder(ctx context.Context, kind string, w signal.Window, reader Reader) (order, error) {
	o := order{raw: historicalSort(kind, w), expr: "activity.score", desc: true, cast: "float8", args: pgx.NamedArgs{}}
	if err := s.signals(); err != nil {
		return o, err
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
	o.ranking = &result.Ranking
	o.rankings = map[string]ContentRanking{}
	ids, scores := make([]string, 0, len(result.Hits)), make([]float64, 0, len(result.Hits))
	for _, h := range result.Hits {
		if h.Score > 0 {
			ids, scores = append(ids, h.ContentID), append(scores, h.Score)
			if h.Normalization != nil {
				o.rankings[h.ContentID] = ContentRanking{Score: h.Score, Participation: h.Normalization.RawRate, AdjustedParticipation: h.Normalization.Rate}
			}
		}
	}
	o.join = "JOIN unnest(@d_activity_ids::text[], @d_activity_scores::float8[]) AS activity(content_id, score) ON activity.content_id = f.content_id"
	o.args["d_activity_ids"], o.args["d_activity_scores"] = ids, scores
	return o, nil
}
