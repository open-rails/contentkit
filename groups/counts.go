package groups

import (
	"context"
	"fmt"
)

// Count is the number of distinct live, listed contents of one kind in a
// group (its descendants included) serving one language; Language "" counts
// every language once.
type Count struct {
	ContentKind string `json:"content_kind"`
	Language    string `json:"language"`
	Count       int    `json:"count"`
}

// liveListed is the facet predicate counts and automatic covers share.
const liveListed = `NOT f.deleted AND f.listing = 'listed' AND f.live_from IS NOT NULL AND f.live_from <= now()`

// Recount rebuilds content_group_counts from content_facets, whose rows are
// versions: per active group, content kind and language, the distinct live,
// listed contents. It also re-picks every automatic cover: the
// most-favorited live, listed member. It is an engine job; counts may lag
// writes until it runs.
func (s *Store) Recount(ctx context.Context) error {
	return s.run(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, fmt.Sprintf(`WITH c AS (
 SELECT gid AS group_id, f.kind AS content_kind, l.language, count(DISTINCT f.content_id)::int AS n
 FROM %[1]s f CROSS JOIN LATERAL unnest(f.group_ids) AS gid CROSS JOIN LATERAL unnest(array_append(f.languages, '')) AS l(language)
 JOIN %[2]s g ON g.tenant_id = f.tenant_id AND g.id = gid AND g.state = 'active'
 WHERE f.tenant_id = $1 AND %[4]s
 GROUP BY 1, 2, 3),
up AS (
 INSERT INTO %[3]s AS gc (tenant_id, group_id, content_kind, language, content_count)
 SELECT $1, group_id, content_kind, language, n FROM c
 ON CONFLICT (tenant_id, group_id, content_kind, language) DO UPDATE SET content_count = EXCLUDED.content_count, updated_at = now()
 WHERE gc.content_count <> EXCLUDED.content_count)
DELETE FROM %[3]s gc WHERE gc.tenant_id = $1
 AND NOT EXISTS (SELECT 1 FROM c WHERE c.group_id = gc.group_id AND c.content_kind = gc.content_kind AND c.language = gc.language)`,
			s.table("content_facets"), s.table("content_groups"), s.table("content_group_counts"), liveListed), s.tenant); err != nil {
			return err
		}
		_, err := q.Exec(ctx, fmt.Sprintf(`WITH best AS (
 SELECT DISTINCT ON (gid) gid AS group_id, f.content_id
 FROM %[1]s f CROSS JOIN LATERAL unnest(f.group_ids) AS gid
 WHERE f.tenant_id = $1 AND %[3]s
 ORDER BY gid, f.favorites DESC, f.published_at DESC NULLS LAST, f.content_id),
auto AS (
 SELECT g.id, CASE WHEN b.content_id IS NULL THEN NULL ELSE jsonb_build_object('content_id', b.content_id, 'auto', true) END AS cover
 FROM %[2]s g LEFT JOIN best b ON b.group_id = g.id
 WHERE g.tenant_id = $1 AND g.state = 'active' AND (g.cover IS NULL OR g.cover ? 'auto'))
UPDATE %[2]s g SET cover = auto.cover FROM auto
 WHERE g.tenant_id = $1 AND g.id = auto.id AND g.cover IS DISTINCT FROM auto.cover`,
			s.table("content_facets"), s.table("content_groups"), liveListed), s.tenant)
		return err
	})
}

func (s *Store) counts(ctx context.Context, q Querier, ids []string) (map[GroupID][]Count, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT group_id, content_kind, language, content_count FROM %s
 WHERE tenant_id = $1 AND group_id = ANY($2::text[]) ORDER BY group_id, content_kind, language`, s.table("content_group_counts")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[GroupID][]Count{}
	for rows.Next() {
		var id string
		var c Count
		if err := rows.Scan(&id, &c.ContentKind, &c.Language, &c.Count); err != nil {
			return nil, err
		}
		out[GroupID(id)] = append(out[GroupID(id)], c)
	}
	return out, rows.Err()
}
