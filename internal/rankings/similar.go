package rankings

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// similarTTL is how long a content's similar list is cached: it follows the
// catalog, not activity.
const similarTTL = 6 * time.Hour

// similarDepth bounds a cached similar list; viewers' filters take from it.
const similarDepth = 200

const (
	// similarEntities are the anchor's most specific entities a similar
	// list draws on.
	similarEntities = 12
	// similarPerEntity bounds the contents read per entity, newest first.
	similarPerEntity = 2000
	// artistWeight is added to a shared artist's weight.
	artistWeight = 2.0
)

// Similar are contents of kind like the anchor, best first, without it: those
// sharing its most specific entities, each weighted by its IDF (rarer
// entities count more), a shared artist counting extra. Computed in Postgres
// and cached per content; the caller applies the viewer's filters.
func (e *Engine) Similar(ctx context.Context, kind, anchor string) ([]string, error) {
	key := "similar:" + kind + ":" + anchor
	if raw, ok, err := e.store.get(ctx, key); err != nil {
		return nil, err
	} else if ok {
		return decodeIDs(raw), nil
	}
	n, err := e.kindSize(ctx, kind)
	if err != nil {
		return nil, err
	}
	var artists []string
	for name, k := range e.o.Kinds.EntityKinds {
		if k.Artist {
			artists = append(artists, name)
		}
	}
	slices.Sort(artists)
	memberships, entities, facets := e.table("content_memberships"), e.table("content_entities"), e.table("content_facets")
	rows, err := e.o.Pool.Query(ctx, `WITH anchor AS (
 SELECT DISTINCT a.entity_id FROM `+facets+` f CROSS JOIN LATERAL unnest(f.entity_ids) a(entity_id)
 WHERE f.tenant_id = $1 AND f.content_id = $2 AND NOT f.deleted
), weighted AS (
 SELECT a.entity_id, e.kind = ANY($4::text[]) AS artist, ln(1 + $3::float8 / greatest(df.n, 1)) AS w
 FROM anchor a JOIN `+entities+` e ON e.tenant_id = $1 AND e.id = a.entity_id AND e.state = 'active' AND e.kind <> 'channel'
 CROSS JOIN LATERAL (SELECT count(DISTINCT m.content_id) AS n FROM `+memberships+` m
  WHERE m.tenant_id = $1 AND m.entity_id = a.entity_id AND m.state = 'active') df
), chosen AS (
 SELECT entity_id, w + CASE WHEN artist THEN $5::float8 ELSE 0 END AS w FROM weighted ORDER BY artist DESC, w DESC, entity_id LIMIT $6
), shared AS (
 SELECT c.content_id, sum(ch.w) AS score FROM chosen ch
 CROSS JOIN LATERAL (SELECT DISTINCT m.content_id FROM `+memberships+` m
  WHERE m.tenant_id = $1 AND m.entity_id = ch.entity_id AND m.state = 'active' AND m.content_id <> $2
  ORDER BY m.content_id DESC LIMIT $7) c
 GROUP BY c.content_id
) SELECT s.content_id FROM shared s
 WHERE EXISTS (SELECT 1 FROM `+facets+` f WHERE f.tenant_id = $1 AND f.content_id = s.content_id AND f.kind = $8
  AND NOT f.deleted AND f.live_from IS NOT NULL)
 ORDER BY s.score DESC, s.content_id DESC LIMIT $9`,
		e.o.Tenant, anchor, float64(n), artists, artistWeight, similarEntities, similarPerEntity, kind, similarDepth)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return ids, e.store.set(ctx, key, encodeIDs(ids), similarTTL)
}

// kindSize is a kind's content count, the N of the IDF weights, cached an
// hour.
type kindSize struct {
	n  int
	at time.Time
}

func (e *Engine) kindSize(ctx context.Context, kind string) (int, error) {
	key := kind
	if v, ok := e.sizes.Load(key); ok && time.Since(v.(kindSize).at) < time.Hour {
		return v.(kindSize).n, nil
	}
	var n int
	err := e.o.Pool.QueryRow(ctx, `SELECT count(DISTINCT content_id) FROM `+e.table("content_facets")+` WHERE tenant_id = $1 AND kind = $2 AND NOT deleted`, e.o.Tenant, kind).Scan(&n)
	if err == nil {
		e.sizes.Store(key, kindSize{n, time.Now()})
	}
	return n, err
}

func encodeIDs(ids []string) []byte {
	var b bytes.Buffer
	for _, id := range ids {
		u := uuid.MustParse(id)
		b.Write(u[:])
	}
	return b.Bytes()
}

func decodeIDs(raw []byte) []string {
	out := make([]string, 0, len(raw)/16)
	for p := 0; p+16 <= len(raw); p += 16 {
		u, _ := uuid.FromBytes(raw[p : p+16])
		out = append(out, u.String())
	}
	return out
}
