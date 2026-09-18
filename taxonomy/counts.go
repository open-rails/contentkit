package taxonomy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// Counts returns the per-kind, per-language counts of the given nodes.
func (s *Store) Counts(ctx context.Context, ids []TaxonomyID) (map[TaxonomyID][]Count, error) {
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := map[TaxonomyID][]Count{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q querier) error {
		out, err = s.counts(ctx, q, list)
		return err
	})
	return out, err
}

func (s *Store) counts(ctx context.Context, q querier, ids []string) (map[TaxonomyID][]Count, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT taxonomy_id, content_kind, language, content_count FROM %s
 WHERE tenant_id=$1 AND taxonomy_id=ANY($2::text[]) ORDER BY taxonomy_id, content_kind, language`, s.table("content_node_counts")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[TaxonomyID][]Count{}
	for rows.Next() {
		var id string
		var c Count
		if err := rows.Scan(&id, &c.ContentKind, &c.Language, &c.Count); err != nil {
			return nil, err
		}
		out[TaxonomyID(id)] = append(out[TaxonomyID(id)], c)
	}
	return out, rows.Err()
}

// recountNodes recomputes the counts of the given nodes from the tenant's
// documents joined with the count eligibility; callers hold the node locks.
func (s *Store) recountNodes(ctx context.Context, q querier, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := pgx.NamedArgs{"tenant": s.tenant, "ids": ids, "taxonomy_kinds": s.kindList}
	join, _, err := search.EligibilityJoin(s.countElig, args)
	if err != nil {
		return fmt.Errorf("%w: CountEligibility: %v", ErrInvalid, err)
	}
	counts := s.table("content_node_counts")
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id=@tenant AND taxonomy_id=ANY(@ids::text[])`, counts), args); err != nil {
		return err
	}
	_, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, taxonomy_id, content_kind, language, content_count, updated_at)
 SELECT @tenant, a.taxonomy_id, sd.content_kind, sd.language, count(DISTINCT sd.content_id), now()
 FROM %s.content_search_documents sd%s
 JOIN %s a ON a.tenant_id=sd.tenant_id AND a.content_kind=sd.content_kind AND a.content_id=sd.content_id
   AND coalesce(a.content_version_id,'') IN ('', sd.content_version_id) AND a.state='active' AND a.taxonomy_id=ANY(@ids::text[])
 JOIN %s n ON n.tenant_id=a.tenant_id AND n.taxonomy_id=a.taxonomy_id AND n.state='active'
 WHERE sd.tenant_id=@tenant AND sd.content_kind<>ALL(@taxonomy_kinds::text[])
 GROUP BY a.taxonomy_id, sd.content_kind, sd.language`, counts, s.qs, join, s.table("content_assignments"), s.table("content_nodes")), args)
	return err
}

// RecountNodes recomputes the counts of the given nodes.
func (s *Store) RecountNodes(ctx context.Context, ids []TaxonomyID) error {
	list, err := uniqueIDs(ids)
	if err != nil || len(list) == 0 {
		return err
	}
	return s.run(ctx, func(q querier) error {
		if err := s.lockNodes(ctx, q, list); err != nil {
			return err
		}
		return s.recountNodes(ctx, q, list)
	})
}

// RecountContent recomputes the counts of every node assigned to the given
// works (any version). Hosts call it when a work's versions, languages or
// visibility change.
func (s *Store) RecountContent(ctx context.Context, refs []contentref.ContentRef) error {
	if len(refs) == 0 {
		return nil
	}
	data, err := s.refRows(refs)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT a.taxonomy_id FROM %s a JOIN jsonb_to_recordset($2::jsonb) AS r(content_kind text, content_id text)
 ON r.content_kind=a.content_kind AND r.content_id=a.content_id WHERE a.tenant_id=$1 ORDER BY a.taxonomy_id`, s.table("content_assignments")), s.tenant, data)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return err
		}
		if err := s.lockNodes(ctx, q, ids); err != nil {
			return err
		}
		return s.recountNodes(ctx, q, ids)
	})
}

// RebuildCounts recomputes every node's counts of the tenant in pages and
// returns the number of nodes rebuilt. Use it after suppressed bulk writes,
// document backfills or an eligibility change.
func (s *Store) RebuildCounts(ctx context.Context) (int, error) {
	const pageSize = 1000
	cursor, total := "", 0
	for {
		var ids []string
		err := s.run(ctx, func(q querier) error {
			rows, err := q.Query(ctx, fmt.Sprintf(`SELECT taxonomy_id FROM %s WHERE tenant_id=$1 AND taxonomy_id>$2 ORDER BY taxonomy_id LIMIT $3 FOR UPDATE`, s.table("content_nodes")), s.tenant, cursor, pageSize)
			if err != nil {
				return err
			}
			if ids, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(ids) == 0 {
				return err
			}
			return s.recountNodes(ctx, q, ids)
		})
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		total += len(ids)
		cursor = ids[len(ids)-1]
	}
}
