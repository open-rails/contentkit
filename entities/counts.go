package entities

import (
	"context"
	"fmt"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
)

// Count is the number of distinct live, listed contents of one kind in a
// entity (its descendants included) or owned by it, serving one language;
// Language "" counts every language once.
type Count struct {
	ContentKind string `json:"content_kind"`
	Language    string `json:"language"`
	Count       int    `json:"count"`
}

// liveListed is the facet predicate counts and automatic covers share.
const liveListed = `NOT f.deleted AND NOT f.unplayable AND f.live_from IS NOT NULL AND f.live_from <= now()`

// Recount rebuilds content_entity_counts from content_facets, whose rows are
// versions and whose entity_ids already include ancestors: per active entity,
// content kind and language, the distinct live, listed contents it holds or
// owns. It also re-picks every automatic cover: the most-favorited live,
// listed content it holds or owns whose cover anyone sees (free, or its kind's
// locked view shows covers). It is an engine job; counts and automatic covers
// lag writes until it runs.
func (s *Store) Recount(ctx context.Context) error {
	return s.run(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, fmt.Sprintf(`WITH c AS (
 SELECT eid AS entity_id, f.kind AS content_kind, l.language, count(DISTINCT f.content_id)::int AS n
 FROM %[1]s f CROSS JOIN LATERAL unnest(array_append(f.entity_ids, f.owner_entity_id)) AS eid CROSS JOIN LATERAL unnest(array_append(f.languages, '')) AS l(language)
 JOIN %[2]s g ON g.tenant_id = f.tenant_id AND g.id = eid AND g.state = 'active'
 WHERE f.tenant_id = $1 AND %[4]s
 GROUP BY 1, 2, 3),
up AS (
 INSERT INTO %[3]s AS ec (tenant_id, entity_id, content_kind, language, content_count)
 SELECT $1, entity_id, content_kind, language, n FROM c
 ON CONFLICT (tenant_id, entity_id, content_kind, language) DO UPDATE SET content_count = EXCLUDED.content_count, updated_at = now()
 WHERE ec.content_count <> EXCLUDED.content_count)
DELETE FROM %[3]s ec WHERE ec.tenant_id = $1
 AND NOT EXISTS (SELECT 1 FROM c WHERE c.entity_id = ec.entity_id AND c.content_kind = ec.content_kind AND c.language = ec.language)`,
			s.table("content_facets"), s.table("content_entities"), s.table("content_entity_counts"), liveListed), s.tenant); err != nil {
			return err
		}
		_, err := q.Exec(ctx, fmt.Sprintf(`WITH best AS (
 SELECT DISTINCT ON (eid) eid AS entity_id, f.content_id
 FROM %[1]s f CROSS JOIN LATERAL unnest(array_append(f.entity_ids, f.owner_entity_id)) AS eid
 LEFT JOIN %[4]s counts ON counts.tenant_id = f.tenant_id AND counts.content_kind = f.kind AND counts.content_id = f.content_id AND counts.content_version_id IS NULL
 WHERE f.tenant_id = $1 AND eid IS NOT NULL AND %[3]s
  AND (%[5]s OR f.kind = ANY($2::text[]))
 ORDER BY eid, coalesce(counts.favorites, 0) DESC, f.published_at DESC NULLS LAST, f.content_id),
auto AS (
 SELECT g.id, CASE WHEN b.content_id IS NULL THEN NULL ELSE jsonb_build_object('content_id', b.content_id, 'auto', true) END AS cover
 FROM %[2]s g LEFT JOIN best b ON b.entity_id = g.id
 WHERE g.tenant_id = $1 AND g.state = 'active' AND (g.cover IS NULL OR g.cover ? 'auto'))
UPDATE %[2]s g SET cover = auto.cover FROM auto
 WHERE g.tenant_id = $1 AND g.id = auto.id AND g.cover IS DISTINCT FROM auto.cover`,
			s.table("content_facets"), s.table("content_entities"), liveListed, s.table("content_interaction_counts"), access.PublicSQL("f")), s.tenant, s.coveredKinds())
		return err
	})
}

// RefreshWindows re-derives the facets of content whose timed membership
// opened or closed after its facet row was written. Comparing against the
// row's own write time instead of a lookback means a run after any outage
// still catches every boundary it missed. It is an engine job.
func (s *Store) RefreshWindows(ctx context.Context) error {
	return s.run(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT f.content_id FROM %s m
 JOIN %s f ON f.tenant_id = m.tenant_id AND f.content_id = m.content_id AND (m.version_id IS NULL OR m.version_id = f.version_id)
 WHERE m.tenant_id = $1 AND (m.active_from IS NOT NULL OR m.active_until IS NOT NULL)
  AND ((m.active_from <= now() AND m.active_from > f.updated_at) OR (m.active_until <= now() AND m.active_until > f.updated_at))
 ORDER BY 1`, s.table("content_memberships"), s.table("content_facets")), s.tenant)
		if err != nil {
			return err
		}
		ids, err := collectStrings(rows)
		if err != nil || len(ids) == 0 {
			return err
		}
		if err := s.refreshFacets(ctx, q, ids); err != nil {
			return err
		}
		// A boundary need not change entity_ids (another membership may hold
		// the same entity); stamp the rows so they are not selected again.
		_, err = q.Exec(ctx, `UPDATE `+s.table("content_facets")+` SET updated_at = now() WHERE tenant_id = $1 AND content_id = ANY($2::text[])`, s.tenant, ids)
		return err
	})
}

// coveredKinds are the content kinds whose locked view shows covers.
func (s *Store) coveredKinds() []string {
	out := []string{}
	for name, k := range s.cfg.Kinds {
		if k.LockedView().Shows(kinds.ShowCover) {
			out = append(out, name)
		}
	}
	return out
}

func (s *Store) counts(ctx context.Context, q Querier, ids []string) (map[EntityID][]Count, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT entity_id, content_kind, language, content_count FROM %s
 WHERE tenant_id = $1 AND entity_id = ANY($2::text[]) ORDER BY entity_id, content_kind, language`, s.table("content_entity_counts")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[EntityID][]Count{}
	for rows.Next() {
		var id string
		var c Count
		if err := rows.Scan(&id, &c.ContentKind, &c.Language, &c.Count); err != nil {
			return nil, err
		}
		out[EntityID(id)] = append(out[EntityID(id)], c)
	}
	return out, rows.Err()
}
