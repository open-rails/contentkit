package groups

import (
	"context"
	"fmt"
	"github.com/open-rails/contentkit/access"

	"github.com/open-rails/contentkit/kinds"
)

// Count is the number of distinct live, listed contents of one kind in a
// group (its descendants included) or owned by it, serving one language;
// Language "" counts every language once.
type Count struct {
	ContentKind string `json:"content_kind"`
	Language    string `json:"language"`
	Count       int    `json:"count"`
}

// liveListed is the facet predicate counts and automatic covers share.
const liveListed = `NOT f.deleted AND NOT f.unplayable AND f.live_from IS NOT NULL AND f.live_from <= now()`

// coverSQL derives an automatic cover from live members at read time. Explicit
// covers stay untouched. The query uses aggregate content favorites, never a
// version's reactions, and preserves the public/locked-cover visibility rule.
func (s *Store) coverSQL() string {
	return fmt.Sprintf(`CASE WHEN g.cover IS NOT NULL AND NOT g.cover ? 'auto' THEN g.cover
 WHEN g.state='active' THEN (SELECT jsonb_build_object('content_id',f.content_id,'auto',true)
 FROM %s f LEFT JOIN %s counts ON counts.tenant_id=f.tenant_id AND counts.content_kind=f.kind
 AND counts.content_id=f.content_id AND counts.content_version_id IS NULL
 WHERE f.tenant_id=g.tenant_id AND (f.group_ids @> ARRAY[g.id] OR f.owner_group_id=g.id) AND %s
 AND (%s OR f.kind=ANY(@covered_kinds::text[]))
 ORDER BY coalesce(counts.favorites,0) DESC,f.published_at DESC NULLS LAST,f.content_id LIMIT 1)
 ELSE NULL END`, s.table("content_current_facets"), s.table("content_interaction_counts"), liveListed, access.PublicSQL("f"))
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

func (s *Store) counts(ctx context.Context, q Querier, ids []string) (map[GroupID][]Count, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.id,f.kind,l.language,count(DISTINCT f.content_id)::int
 FROM %s g JOIN %s f ON f.tenant_id=g.tenant_id AND (f.group_ids @> ARRAY[g.id] OR f.owner_group_id=g.id)
 CROSS JOIN LATERAL unnest(array_append(f.languages,'')) l(language)
 WHERE g.tenant_id=$1 AND g.id=ANY($2::text[]) AND g.state='active' AND %s
 GROUP BY g.id,f.kind,l.language ORDER BY g.id,f.kind,l.language`, s.table("content_groups"), s.table("content_current_facets"), liveListed), s.tenant, ids)
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
