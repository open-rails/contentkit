package discovery

import (
	"context"
	"encoding/json"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// PopularityDepth bounds the contents per kind and period that carry a
// popularity score; the rest sort as unviewed.
const PopularityDepth = 100_000

// RefreshPopularity copies the signal plane's popularity into
// content_facets.popularity, so popular and windowed top_rated sorts are one
// Postgres query under any filter: each period's policy score, each window's
// rating (the Wilson lower bound of the subjects whose feedback in it is
// positive) and all-time viewers. Run it periodically (the engine's
// contentkit_popularity job).
func (s *Service) RefreshPopularity(ctx context.Context) error {
	if err := s.signals(); err != nil {
		return err
	}
	now := s.now()
	for _, kind := range s.kindNames() {
		facets := map[string]*popularityFacet{}
		at := func(id string) *popularityFacet {
			f := facets[id]
			if f == nil {
				f = &popularityFacet{Score: map[string]float64{}, Rating: map[string]float64{}}
				facets[id] = f
			}
			return f
		}
		for _, period := range popularity.Periods {
			window, err := popularity.WindowForPeriod(period, now)
			if err != nil {
				return err
			}
			hits, err := s.o.Hub.Popular(ctx, kind, signal.PopularOptions{Window: window, Limit: PopularityDepth, RankExpr: s.o.Policy.RankExpr()})
			if err != nil {
				return err
			}
			for _, h := range hits {
				f := at(h.ContentID)
				if h.Score > 0 {
					f.Score[period] = h.Score
				}
				if r := wilson(h.PositiveSubjects, h.NegativeSubjects); r > 0 {
					f.Rating[period] = r
				}
				if period == "all" {
					f.Views = int64(h.Viewers)
				}
			}
		}
		if err := s.writePopularity(ctx, kind, facets); err != nil {
			return err
		}
	}
	return nil
}

// wilson is the 95% Wilson lower bound of positive among rated subjects.
func wilson(positive, negative uint64) float64 {
	n := float64(positive + negative)
	if n == 0 {
		return 0
	}
	p := float64(positive) / n
	const z = 1.96
	return (p + z*z/(2*n) - z*math.Sqrt((p*(1-p)+z*z/(4*n))/n)) / (1 + z*z/n)
}

// writePopularity sets every version's popularity of the kind's contents in
// one transaction, clearing it where the signal plane has none.
func (s *Service) writePopularity(ctx context.Context, kind string, facets map[string]*popularityFacet) error {
	type row struct {
		ID string          `json:"content_id"`
		P  json.RawMessage `json:"p"`
	}
	rows := make([]row, 0, len(facets))
	for id, f := range facets {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		rows = append(rows, row{id, b})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.facets+` f SET popularity = n.p, updated_at = clock_timestamp()
 FROM jsonb_to_recordset($3::jsonb) AS n(content_id text, p jsonb)
 WHERE f.tenant_id = $1 AND f.kind = $2 AND f.content_id = n.content_id AND f.popularity IS DISTINCT FROM n.p`, s.o.Tenant, kind, data); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE `+s.t.facets+` f SET popularity = '{}', updated_at = clock_timestamp()
 WHERE f.tenant_id = $1 AND f.kind = $2 AND f.popularity <> '{}' AND NOT (f.content_id = ANY($3::text[]))`, s.o.Tenant, kind, keys(facets))
		return err
	})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
