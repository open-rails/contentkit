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
// one of the artist's galleries was good or complete before the next one was
// released, and who were active after that release, the share who read the
// next one within readThroughDays of it. Only pairs whose window has closed
// count.
type ReadThrough struct {
	Rate    float64
	Readers int
}

const readThroughDays = 30

// readThroughTTL caches an artist's rate; it moves by the day.
const readThroughTTL = 12 * time.Hour

// ReadThrough is each artist's rate, from reading_state; absent for an
// artist without a closed pair read.
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
 SELECT a.id AS artist, f.content_id::uuid AS content_id, min(f.live_from) AS released
 FROM unnest($2::text[]) a(id) JOIN `+e.table("content_facets")+` f ON f.tenant_id = $1 AND (f.entity_ids @> ARRAY[a.id] OR f.owner_entity_id = a.id)
 WHERE NOT f.deleted AND f.live_from IS NOT NULL
 GROUP BY a.id, f.content_id
), pairs AS (
 SELECT artist, content_id, next, next_released FROM (
  SELECT artist, content_id, lead(content_id) OVER w AS next, lead(released) OVER w AS next_released
  FROM galleries WINDOW w AS (PARTITION BY artist ORDER BY released, content_id)) p
 WHERE next IS NOT NULL AND next_released + make_interval(days => $4) <= $5
), readers AS (
 SELECT p.artist, p.next, p.next_released, r.subject FROM pairs p
 JOIN `+state+` r ON r.app_id = $3::uuid AND r.content_id = p.content_id
 WHERE r.best_outcome >= 3 AND r.first_day < (p.next_released AT TIME ZONE 'UTC')::date
 AND EXISTS (SELECT 1 FROM `+state+` a WHERE a.app_id = $3::uuid AND a.subject = r.subject AND a.last_seen_at >= p.next_released)
) SELECT artist, count(*)::int, count(*) FILTER (WHERE EXISTS (SELECT 1 FROM `+state+` n WHERE n.app_id = $3::uuid
  AND n.subject = readers.subject AND n.content_id = readers.next
  AND n.first_day <= ((readers.next_released + make_interval(days => $4)) AT TIME ZONE 'UTC')::date))::int
 FROM readers GROUP BY artist`, e.o.Tenant, missing, e.o.Tenant, readThroughDays, e.now())
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
