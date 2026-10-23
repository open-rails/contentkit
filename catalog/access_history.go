package catalog

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// recordAccessHistory keeps a version's availability and effective access rule
// in the same transaction as its catalog change. Equal snapshots need no new
// row: unrelated edits cannot split a ranking interval or change its weight.
func (s *Store) recordAccessHistory(ctx context.Context, q Querier, ids []string) error {
	history := pgx.Identifier{s.schema, "content_access_history"}.Sanitize()
	start := pgx.Identifier{s.schema, "content_access_history_start"}.Sanitize()
	at := s.now().UTC()
	if _, err := q.Exec(ctx, `INSERT INTO `+start+` (tenant_id, started_at) VALUES ($1,$2)
 ON CONFLICT (tenant_id) DO NOTHING`, s.tenant, at); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO `+history+` (tenant_id, content_id, version_id, occurred_at, snapshot)
 SELECT f.tenant_id, f.content_id, f.version_id, $3, state.snapshot
 FROM `+s.t.facets+` f
 CROSS JOIN LATERAL (SELECT jsonb_build_object(
  'kind', f.kind, 'live_from', f.live_from, 'deleted', f.deleted, 'unplayable', f.unplayable,
  'languages', f.languages,
  'level', f.access_level, 'tiers', f.access_tiers,
  'group_id', f.access_group_id, 'releases', f.access_releases) AS snapshot) state
 LEFT JOIN LATERAL (SELECT h.snapshot FROM `+history+` h
  WHERE h.tenant_id=f.tenant_id AND h.version_id=f.version_id
  ORDER BY h.occurred_at DESC,h.id DESC LIMIT 1) previous ON true
 WHERE f.tenant_id=$1 AND f.content_id=ANY($2::text[])
  AND previous.snapshot IS DISTINCT FROM state.snapshot`, s.tenant, ids, at)
	return err
}
