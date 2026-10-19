package groups

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
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
UPDATE %s f SET group_ids = eff.group_ids, updated_at = now() FROM eff
 WHERE f.tenant_id = $1 AND f.version_id = eff.version_id AND f.group_ids IS DISTINCT FROM eff.group_ids`, s.table("content_facets")), s.tenant, contentIDs); err != nil {
		return err
	}
	if s.changed == nil {
		return nil
	}
	return s.changed(ctx, q, contentIDs)
}

// A group write refreshes up to inlineRefresh reached contents in its own
// transaction. A larger reach is refreshed after it commits by a queued job,
// refreshBatch contents per transaction, so one write never locks a whole tag.
var (
	inlineRefresh = 500
	refreshBatch  = 200
)

// refreshGroupFacets re-derives the facets of every content a change to the
// given groups can reach: members of them or of their descendants, and
// content whose facets name any of them.
func (s *Store) refreshGroupFacets(ctx context.Context, q Querier, groupIDs []string) error {
	if len(groupIDs) == 0 {
		return nil
	}
	contentIDs, err := s.groupReach(ctx, q, groupIDs)
	if err != nil {
		return err
	}
	if len(contentIDs) > inlineRefresh {
		if queued, err := s.queueRefresh(ctx, q, groupIDs); err != nil || queued {
			return err
		}
	}
	return s.refreshFacets(ctx, q, contentIDs)
}

func (s *Store) groupReach(ctx context.Context, q Querier, groupIDs []string) ([]string, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`WITH RECURSIVE sub(id) AS (
 SELECT unnest($2::text[])
 UNION
 SELECT g.id FROM %[1]s g JOIN sub ON g.tenant_id = $1 AND g.parent_id = sub.id)
SELECT m.content_id FROM %[2]s m WHERE m.tenant_id = $1 AND m.group_id IN (SELECT id FROM sub)
UNION
SELECT f.content_id FROM %[3]s f WHERE f.tenant_id = $1 AND f.group_ids && (SELECT array_agg(id) FROM sub)
ORDER BY 1`,
		s.table("content_groups"), s.table("content_memberships"), s.table("content_facets")), s.tenant, groupIDs)
	if err != nil {
		return nil, err
	}
	return collectStrings(rows)
}

// queueRefresh queues the groups' member refresh in the writing transaction,
// when River is bound; false leaves it to the caller.
func (s *Store) queueRefresh(ctx context.Context, q Querier, groupIDs []string) (bool, error) {
	client := s.river.bound()
	tx, ok := q.(pgx.Tx)
	if client == nil || !ok {
		return false, nil
	}
	_, err := client.InsertTx(ctx, tx, refreshArgs{Tenant: s.tenant, Groups: groupIDs}, refreshOpts(s.queue))
	return err == nil, err
}

// RefreshGroupMembers re-derives the facets of the contents the groups reach
// whose ids sort after after, refreshBatch per transaction, until budget
// passes (0: none). It returns where it stopped, "" when done. Repeating it
// is harmless.
func (s *Store) RefreshGroupMembers(ctx context.Context, groupIDs []string, after string, budget time.Duration) (string, error) {
	var contentIDs []string
	if err := s.read(ctx, func(q Querier) (err error) {
		contentIDs, err = s.groupReach(ctx, q, groupIDs)
		return err
	}); err != nil {
		return "", err
	}
	start := time.Now()
	contentIDs = contentIDs[sort.SearchStrings(contentIDs, after+"\x00"):]
	for len(contentIDs) > 0 {
		batch := contentIDs[:min(refreshBatch, len(contentIDs))]
		if err := s.run(ctx, func(q Querier) error { return s.refreshFacets(ctx, q, batch) }); err != nil {
			return after, err
		}
		after, contentIDs = batch[len(batch)-1], contentIDs[len(batch):]
		if budget > 0 && len(contentIDs) > 0 && time.Since(start) > budget {
			return after, nil
		}
	}
	return "", nil
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

// Memberships is popularity.Catalog: per content id, the ids of the active
// groups of groupKind its versions are effective in; contents in none are
// absent.
func (s *Store) Memberships(ctx context.Context, tenant, contentKind, groupKind string, contentIDs []string) (map[string][]GroupID, error) {
	if tenant != s.tenant {
		return nil, fmt.Errorf("%w: tenant %s", ErrInvalid, tenant)
	}
	out := map[string][]GroupID{}
	if len(contentIDs) == 0 {
		return out, nil
	}
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, s.effectiveSQL(fmt.Sprintf(`SELECT cv.id AS version_id, cv.content_id FROM %s cv
 JOIN %s c ON c.tenant_id = cv.tenant_id AND c.id = cv.content_id AND c.kind = $3
 WHERE cv.tenant_id = $1 AND cv.content_id = ANY($2::text[])`, s.table("content_versions"), s.table("content_items")))+fmt.Sprintf(`
SELECT v.content_id, array_agg(DISTINCT gid ORDER BY gid) FROM v JOIN eff ON eff.version_id = v.version_id CROSS JOIN LATERAL unnest(eff.group_ids) AS gid
 JOIN %s g ON g.tenant_id = $1 AND g.id = gid AND g.kind = $4 GROUP BY v.content_id`, s.table("content_groups")), s.tenant, contentIDs, contentKind, groupKind)
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
				out[id] = append(out[id], GroupID(g))
			}
		}
		return rows.Err()
	})
	return out, err
}
