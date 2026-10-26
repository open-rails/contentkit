package entities

import (
	"context"
	"fmt"
)

// effectiveSQL derives eff(version_id, entity_ids) for the versions source
// selects as (version_id, content_id): active memberships of the content and
// of that version, in active entities, plus every
// active ancestor through parent_id. $1 is the tenant. UNION stops cycles.
func (s *Store) effectiveSQL(source string) string {
	return fmt.Sprintf(`WITH RECURSIVE v AS (%[1]s),
direct AS (
 SELECT v.version_id, m.entity_id FROM v
 JOIN %[2]s m ON m.tenant_id = $1 AND m.content_id = v.content_id AND (m.version_id IS NULL OR m.version_id = v.version_id)
  AND m.state = 'active'
 JOIN %[3]s g ON g.tenant_id = $1 AND g.id = m.entity_id AND g.state = 'active' AND g.kind <> 'channel'),
up (version_id, entity_id) AS (
 SELECT version_id, entity_id FROM direct
 UNION
 SELECT up.version_id, p.id FROM up
 JOIN %[3]s c ON c.tenant_id = $1 AND c.id = up.entity_id
 JOIN %[3]s p ON p.tenant_id = $1 AND p.id = c.parent_id AND p.state = 'active' AND p.kind <> 'channel'),
eff AS (
 SELECT v.version_id, coalesce(array_agg(DISTINCT up.entity_id ORDER BY up.entity_id) FILTER (WHERE up.entity_id IS NOT NULL), '{}'::text[]) AS entity_ids
 FROM v LEFT JOIN up ON up.version_id = v.version_id GROUP BY v.version_id)`,
		source, s.table("content_memberships"), s.table("content_entities"))
}

// EffectiveEntities returns, per version, the sorted ids of the active entities it
// belongs to through its content's memberships or its own,
// plus each one's active ancestors via parent_id. It runs on q, the caller's
// transaction, so memberships written in it count. Unknown versions are
// absent; a version in no entity maps to an empty list.
func (s *Store) EffectiveEntities(ctx context.Context, q Querier, versionIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(versionIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, s.effectiveSQL(fmt.Sprintf(`SELECT cv.id AS version_id, cv.content_id FROM %s cv WHERE cv.tenant_id = $1 AND cv.id = ANY($2::text[])`,
		s.table("content_versions")))+` SELECT version_id, entity_ids FROM eff`, s.tenant, versionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ids []string
		if err := rows.Scan(&id, &ids); err != nil {
			return nil, err
		}
		out[id] = ids
	}
	return out, rows.Err()
}

// refreshFacets re-derives content_facets.entity_ids of every version of the
// given contents in q and tells Options.ContentChanged.
func (s *Store) refreshFacets(ctx context.Context, q Querier, contentIDs []string) error {
	if len(contentIDs) == 0 {
		return nil
	}
	// Catalog mutations lock content before facets/documents. Take that same
	// order before a membership callback advances the catalog revision, so a
	// concurrent publication cannot hold content while waiting on our facets.
	locked, err := q.Query(ctx, `SELECT id FROM `+s.table("content_items")+` WHERE tenant_id=$1 AND id=ANY($2::text[]) ORDER BY id FOR UPDATE`, s.tenant, contentIDs)
	if err != nil {
		return err
	}
	for locked.Next() {
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, s.effectiveSQL(fmt.Sprintf(`SELECT f.version_id, f.content_id FROM %s f WHERE f.tenant_id = $1 AND f.content_id = ANY($2::text[])`,
		s.table("content_facets")))+fmt.Sprintf(`
UPDATE %s f SET entity_ids = eff.entity_ids, updated_at = now() FROM eff
 WHERE f.tenant_id = $1 AND f.version_id = eff.version_id AND f.entity_ids IS DISTINCT FROM eff.entity_ids`, s.table("content_facets")), s.tenant, contentIDs); err != nil {
		return err
	}
	if s.changed == nil {
		return nil
	}
	return s.changed(ctx, q, contentIDs)
}

// refreshEntityFacets re-derives the facets of every content a change to the
// given entities can reach: members of them or of their descendants, and
// content whose facets name any of them. These entities grant access, so the
// facets must change in the same transaction as the entity mutation.
func (s *Store) refreshEntityFacets(ctx context.Context, q Querier, entityIDs []string) error {
	if len(entityIDs) == 0 {
		return nil
	}
	contentIDs, err := s.entityReach(ctx, q, entityIDs)
	if err != nil {
		return err
	}
	return s.refreshFacets(ctx, q, contentIDs)
}

func (s *Store) entityReach(ctx context.Context, q Querier, entityIDs []string) ([]string, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`WITH RECURSIVE sub(id) AS (
 SELECT unnest($2::text[])
 UNION
 SELECT g.id FROM %[1]s g JOIN sub ON g.tenant_id = $1 AND g.parent_id = sub.id)
SELECT m.content_id FROM %[2]s m WHERE m.tenant_id = $1 AND m.entity_id IN (SELECT id FROM sub)
UNION
SELECT f.content_id FROM %[3]s f WHERE f.tenant_id = $1 AND f.entity_ids && (SELECT array_agg(id) FROM sub)
ORDER BY 1`,
		s.table("content_entities"), s.table("content_memberships"), s.table("content_facets")), s.tenant, entityIDs)
	if err != nil {
		return nil, err
	}
	return collectStrings(rows)
}

func collectStrings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Memberships is popularity.Catalog: per content id, the ids of the active
// entities of groupKind its versions are effective in; contents in none are
// absent.
func (s *Store) Memberships(ctx context.Context, tenant, contentKind, entityKind string, contentIDs []string) (map[string][]EntityID, error) {
	if tenant != s.tenant {
		return nil, fmt.Errorf("%w: tenant %s", ErrInvalid, tenant)
	}
	out := map[string][]EntityID{}
	if len(contentIDs) == 0 {
		return out, nil
	}
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, s.effectiveSQL(fmt.Sprintf(`SELECT cv.id AS version_id, cv.content_id FROM %s cv
 JOIN %s c ON c.tenant_id = cv.tenant_id AND c.id = cv.content_id AND c.kind = $3
 WHERE cv.tenant_id = $1 AND cv.content_id = ANY($2::text[])`, s.table("content_versions"), s.table("content_items")))+fmt.Sprintf(`
SELECT v.content_id, array_agg(DISTINCT gid ORDER BY gid) FROM v JOIN eff ON eff.version_id = v.version_id CROSS JOIN LATERAL unnest(eff.entity_ids) AS gid
 JOIN %s g ON g.tenant_id = $1 AND g.id = gid AND g.kind = $4 GROUP BY v.content_id`, s.table("content_entities")), s.tenant, contentIDs, contentKind, entityKind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var ids []string
			if err := rows.Scan(&id, &ids); err != nil {
				return err
			}
			for _, g := range ids {
				out[id] = append(out[id], EntityID(g))
			}
		}
		return rows.Err()
	})
	return out, err
}
