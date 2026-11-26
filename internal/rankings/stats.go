package rankings

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Exploration is how far a content has been tried: the people who opened it
// and those whose best read of it was good or complete.
type Exploration struct{ Opens, Good int }

// Exploration counts ids' readers from reading_state, today's included:
// what a new content's share of placements runs on.
func (e *Engine) Exploration(ctx context.Context, ids []string) (map[string]Exploration, error) {
	out := make(map[string]Exploration, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := e.o.Pool.Query(ctx, `SELECT content_id::text, count(*) FILTER (WHERE first_day IS NOT NULL)::int, count(*) FILTER (WHERE best_outcome >= 3)::int
 FROM `+e.table("reading_state")+` WHERE app_id = $1::uuid AND content_id = ANY($2::uuid[]) GROUP BY content_id`, e.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	var id string
	var x Exploration
	_, err = pgx.ForEachRow(rows, []any{&id, &x.Opens, &x.Good}, func() error {
		out[id] = x
		return nil
	})
	return out, err
}

// ReadThrough is an artist's read-through rate: of the people whose read of
// one of the artist's galleries was good or complete, the share who went on
// to read another of them, one that already existed or a later one, within
// readThroughDays of that read (the same day counts). A reader counts from
// their first such read, once its window has closed.
type ReadThrough struct {
	Rate    float64
	Readers int
}

const readThroughDays = 30

// readThroughTTL caches an artist's rate; it moves by the day.
const readThroughTTL = 12 * time.Hour

// ReadThrough is each artist's rate, from reading_state; absent for an
// artist without a reader whose window has closed.
func (e *Engine) ReadThrough(ctx context.Context, artists []string) (map[string]ReadThrough, error) {
	out := map[string]ReadThrough{}
	var missing []string
	for _, id := range artists {
		raw, ok, err := e.store.get(ctx, "readthrough:"+id)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, id)
			continue
		}
		if r, ok := decodeReadThrough(raw); ok {
			out[id] = r
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	state := e.table("reading_state")
	rows, err := e.o.Pool.Query(ctx, `WITH galleries AS (
 SELECT DISTINCT a.id AS artist, f.content_id::uuid AS content_id
 FROM unnest($2::text[]) a(id) JOIN `+e.table("content_facets")+` f ON f.tenant_id = $1 AND (f.entity_ids @> ARRAY[a.id] OR f.owner_entity_id = a.id)
 WHERE NOT f.deleted AND f.live_from IS NOT NULL
), firsts AS (
 SELECT DISTINCT ON (g.artist, r.subject) g.artist, r.subject, r.content_id, r.first_day
 FROM galleries g JOIN `+state+` r ON r.app_id = $3::uuid AND r.content_id = g.content_id
 WHERE r.best_outcome >= 3 AND r.first_day IS NOT NULL
 ORDER BY g.artist, r.subject, r.first_day, r.content_id
) SELECT artist, count(*)::int, count(*) FILTER (WHERE EXISTS (SELECT 1 FROM galleries other
  JOIN `+state+` n ON n.app_id = $3::uuid AND n.content_id = other.content_id
  WHERE other.artist = firsts.artist AND n.subject = firsts.subject AND n.content_id <> firsts.content_id
  AND n.first_day >= firsts.first_day AND n.first_day <= firsts.first_day + $4::int))::int
 FROM firsts WHERE firsts.first_day + $4::int <= $5::date GROUP BY artist`, e.o.Tenant, missing, e.o.Tenant, readThroughDays, e.now().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	var artist string
	var readers, followed int
	found := map[string]ReadThrough{}
	_, err = pgx.ForEachRow(rows, []any{&artist, &readers, &followed}, func() error {
		if readers > 0 {
			found[artist] = ReadThrough{Rate: float64(followed) / float64(readers), Readers: readers}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, id := range missing {
		r, ok := found[id]
		if ok {
			out[id] = r
		}
		if err := e.store.set(ctx, "readthrough:"+id, encodeReadThrough(r, ok), readThroughTTL); err != nil {
			return nil, err
		}
	}
	return out, nil
}
