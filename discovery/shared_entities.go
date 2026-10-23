package discovery

import (
	"context"
	"fmt"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// sharedEntities fills similar lists from active artist, series, character
// and tag memberships. Recommender applies visibility and reader exclusions.
type sharedEntities struct{ s *Service }

func (e sharedEntities) Similar(ctx context.Context, anchor contentref.ContentRef, q Query) ([]Candidate, error) {
	s := e.s
	// Bound the indexed membership walk before joining and ranking works:
	// at most 64 anchor entities and 200 memberships per entity, read
	// backwards along content_memberships_entity_idx so a large tag stops
	// early and offers its newest works (content ids are UUIDv7).
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`WITH anchor_entities AS (
 SELECT DISTINCT g.id, CASE WHEN g.kind = 'tag' THEN 1 ELSE 4 END AS weight
 FROM %s m JOIN %s g ON g.tenant_id = m.tenant_id AND g.id = m.entity_id
 WHERE m.tenant_id = $1 AND m.content_id = $2 AND m.state = 'active'
 AND (m.active_from IS NULL OR m.active_from <= now())
 AND (m.active_until IS NULL OR m.active_until > now())
 AND g.state = 'active' AND NOT g.internal AND g.kind IN ('artist', 'series', 'character', 'tag')
 ORDER BY weight DESC, g.id LIMIT 64
), matches AS (
 SELECT DISTINCT a.id, a.weight, m.content_id FROM anchor_entities a
 CROSS JOIN LATERAL (
  SELECT m.content_id FROM %s m
  WHERE m.tenant_id = $1 AND m.entity_id = a.id AND m.content_id <> $2 AND m.state = 'active'
  AND (m.active_from IS NULL OR m.active_from <= now())
  AND (m.active_until IS NULL OR m.active_until > now())
  ORDER BY m.position DESC, m.content_id DESC LIMIT 200
 ) m
)
SELECT i.kind, i.id, sum(m.weight) AS score FROM matches m
JOIN %s i ON i.tenant_id = $1 AND i.id = m.content_id
WHERE cardinality($3::text[]) = 0 OR i.kind = ANY($3::text[])
GROUP BY i.kind, i.id, i.published_at
ORDER BY score DESC, count(*) DESC, i.published_at DESC NULLS LAST, i.id
LIMIT $4`, s.t.memberships, s.t.entities, s.t.memberships, s.t.items),
		s.o.Tenant, anchor.ContentID, nonNil(q.ContentKinds), clampInt(q.Limit*2, 20, 300))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c := Candidate{Ref: contentref.ContentRef{TenantID: s.o.Tenant}}
		if err := rows.Scan(&c.Ref.ContentKind, &c.Ref.ContentID, &c.Score); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (sharedEntities) ForSubject(context.Context, signal.Subject, Query) ([]Candidate, error) {
	return nil, nil
}
