package groups

import (
	"context"
	"fmt"
	"time"
)

// effectiveSQL derives eff(version_id, group_ids) for the versions source
// selects as (version_id, content_id): active memberships of the content and
// of that version inside their window now, in active groups, plus every
// active ancestor through parent_id. $1 is the tenant. UNION stops cycles.
func (s *Store) effectiveSQL(source string) string {
	return fmt.Sprintf(`WITH RECURSIVE v AS (%[1]s),
direct AS (
 SELECT v.version_id, m.group_id FROM v
 JOIN %[2]s m ON m.tenant_id = $1 AND m.content_id = v.content_id AND (m.version_id IS NULL OR m.version_id = v.version_id)
  AND m.state = 'active' AND (m.active_from IS NULL OR m.active_from <= now()) AND (m.active_until IS NULL OR m.active_until > now())
 JOIN %[3]s g ON g.tenant_id = $1 AND g.id = m.group_id AND g.state = 'active'),
up (version_id, group_id) AS (
 SELECT version_id, group_id FROM direct
 UNION
 SELECT up.version_id, p.id FROM up
 JOIN %[3]s c ON c.tenant_id = $1 AND c.id = up.group_id
 JOIN %[3]s p ON p.tenant_id = $1 AND p.id = c.parent_id AND p.state = 'active'),
eff AS (
 SELECT v.version_id, coalesce(array_agg(DISTINCT up.group_id ORDER BY up.group_id) FILTER (WHERE up.group_id IS NOT NULL), '{}'::text[]) AS group_ids
 FROM v LEFT JOIN up ON up.version_id = v.version_id GROUP BY v.version_id)`,
		source, s.table("content_memberships"), s.table("content_groups"))
}

// EffectiveGroups returns, per version, the sorted ids of the active groups it
// belongs to through its content's memberships or its own (windows applied),
// plus each one's active ancestors via parent_id. It runs on q, the caller's
// transaction, so memberships written in it count. Unknown versions are
// absent; a version in no group maps to an empty list.
func (s *Store) EffectiveGroups(ctx context.Context, q Querier, versionIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(versionIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, s.effectiveSQL(fmt.Sprintf(`SELECT cv.id AS version_id, cv.content_id FROM %s cv WHERE cv.tenant_id = $1 AND cv.id = ANY($2::text[])`,
		s.table("content_versions")))+` SELECT version_id, group_ids FROM eff`, s.tenant, versionIDs)
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

// refreshFacets re-derives content_facets.group_ids of every version of the
// given contents in q.
func (s *Store) refreshFacets(ctx context.Context, q Querier, contentIDs []string) error {
	if len(contentIDs) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, s.effectiveSQL(fmt.Sprintf(`SELECT f.version_id, f.content_id FROM %s f WHERE f.tenant_id = $1 AND f.content_id = ANY($2::text[])`,
		s.table("content_facets")))+fmt.Sprintf(`
UPDATE %s f SET group_ids = eff.group_ids, updated_at = now() FROM eff
 WHERE f.tenant_id = $1 AND f.version_id = eff.version_id AND f.group_ids IS DISTINCT FROM eff.group_ids`, s.table("content_facets")), s.tenant, contentIDs)
	return err
}

// refreshGroupFacets re-derives the facets of every content a change to the
// given groups can reach: members of them or of their descendants, and
// content whose facets name any of them.
func (s *Store) refreshGroupFacets(ctx context.Context, q Querier, groupIDs []string) error {
	if len(groupIDs) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`WITH RECURSIVE sub(id) AS (
 SELECT unnest($2::text[])
 UNION
 SELECT g.id FROM %[1]s g JOIN sub ON g.tenant_id = $1 AND g.parent_id = sub.id)
SELECT m.content_id FROM %[2]s m WHERE m.tenant_id = $1 AND m.group_id IN (SELECT id FROM sub)
UNION
SELECT f.content_id FROM %[3]s f WHERE f.tenant_id = $1 AND f.group_ids && (SELECT array_agg(id) FROM sub)`,
		s.table("content_groups"), s.table("content_memberships"), s.table("content_facets")), s.tenant, groupIDs)
	if err != nil {
		return err
	}
	contentIDs, err := collectStrings(rows)
	if err != nil {
		return err
	}
	return s.refreshFacets(ctx, q, contentIDs)
}

// RefreshWindows re-derives the facets of content whose windowed memberships
// opened or closed within the last lookback. It is an engine job: run it more
// often than lookback; overlapping runs are harmless.
func (s *Store) RefreshWindows(ctx context.Context, lookback time.Duration) error {
	if lookback <= 0 {
		return fmt.Errorf("%w: lookback must be positive", ErrInvalid)
	}
	return s.run(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT content_id FROM %s
 WHERE tenant_id = $1 AND (active_from IS NOT NULL OR active_until IS NOT NULL)
   AND ((active_from > now() - $2 * interval '1 second' AND active_from <= now()) OR (active_until > now() - $2 * interval '1 second' AND active_until <= now()))`,
			s.table("content_memberships")), s.tenant, lookback.Seconds())
		if err != nil {
			return err
		}
		ids, err := collectStrings(rows)
		if err != nil {
			return err
		}
		return s.refreshFacets(ctx, q, ids)
	})
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
