package rankings

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// catalog is the gallery → entity mapping a precompute rolls entities up
// through, held in memory and reloaded when the catalog changes.
type catalog struct {
	fingerprint string
	contents    map[string]catalogContent
	// entityKind is every active public entity's kind.
	entityKind map[string]string
}

type catalogContent struct {
	kind string
	// liveFrom is when its first version went live, which a re-publication
	// never moves; opens is when its earliest open version opens now;
	// published orders ties.
	liveFrom, opens, published time.Time
	entities                   []string
}

// loadCatalog returns the mapping, reloaded when a content or entity row
// changed since it was read.
func (e *Engine) loadCatalog(ctx context.Context) (*catalog, error) {
	e.catalogMu.Lock()
	defer e.catalogMu.Unlock()
	var fingerprint string
	err := e.o.Pool.QueryRow(ctx, `SELECT (SELECT count(*)::text || '/' || coalesce(max(updated_at)::text, '') FROM `+e.table("content_facets")+` WHERE tenant_id = $1)
 || ';' || (SELECT count(*)::text || '/' || coalesce(max(updated_at)::text, '') FROM `+e.table("content_entities")+` WHERE tenant_id = $1)`, e.o.Tenant).Scan(&fingerprint)
	if err != nil {
		return nil, err
	}
	if e.catalog != nil && e.catalog.fingerprint == fingerprint {
		return e.catalog, nil
	}
	c := &catalog{fingerprint: fingerprint, contents: map[string]catalogContent{}, entityKind: map[string]string{}}
	rows, err := e.o.Pool.Query(ctx, `SELECT f.content_id, f.kind, min(f.published_at), min(f.live_from), max(f.published_at),
 coalesce(array_agg(DISTINCT m.entity_id) FILTER (WHERE m.entity_id IS NOT NULL), '{}')
 FROM `+e.table("content_facets")+` f LEFT JOIN LATERAL unnest(array_append(f.entity_ids, f.owner_entity_id)) m(entity_id) ON true
 WHERE f.tenant_id = $1 AND NOT f.deleted AND f.live_from IS NOT NULL
 GROUP BY f.content_id, f.kind`, e.o.Tenant)
	if err != nil {
		return nil, err
	}
	interned := map[string]string{}
	var id, kind string
	var liveFrom, opens time.Time
	var published *time.Time
	var members []string
	_, err = pgx.ForEachRow(rows, []any{&id, &kind, &liveFrom, &opens, &published, &members}, func() error {
		x := catalogContent{kind: kind, liveFrom: liveFrom.UTC(), opens: opens.UTC(), entities: make([]string, len(members))}
		if published != nil {
			x.published = published.UTC()
		}
		for i, m := range members {
			if s, ok := interned[m]; ok {
				m = s
			} else {
				interned[m] = m
			}
			x.entities[i] = m
		}
		c.contents[id] = x
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = e.o.Pool.Query(ctx, `SELECT id, kind FROM `+e.table("content_entities")+` WHERE tenant_id = $1 AND state = 'active' AND NOT internal`, e.o.Tenant)
	if err != nil {
		return nil, err
	}
	_, err = pgx.ForEachRow(rows, []any{&id, &kind}, func() error {
		c.entityKind[id] = kind
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.catalog = c
	return c, nil
}
