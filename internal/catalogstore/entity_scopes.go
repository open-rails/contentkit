package catalogstore

import (
	"context"
	"slices"

	catalog "github.com/open-rails/contentkit/catalog"
)

type entityScope struct{ content, version string }

// entityScopes reads actual ownership once for the visible batch. Facet entity
// IDs deliberately combine parent and version associations for access/search;
// that union must not erase credit ownership in public content details.
func (s *Store) entityScopes(ctx context.Context, ids []string) (map[entityScope][]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT content_id, COALESCE(version_id, ''), entity_id
FROM `+s.t.memberships+` WHERE tenant_id=$1 AND content_id=ANY($2) AND state='active'`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[entityScope][]string{}
	for rows.Next() {
		var scope entityScope
		var id string
		if err := rows.Scan(&scope.content, &scope.version, &id); err != nil {
			return nil, err
		}
		out[scope] = append(out[scope], id)
	}
	return out, rows.Err()
}

func scopedEntities(ids []string, names map[string]catalog.EntitySummary) []catalog.EntitySummary {
	out := make([]catalog.EntitySummary, 0, len(ids))
	for _, id := range ids {
		if n, ok := names[id]; ok {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b catalog.EntitySummary) int {
		if a.Kind != b.Kind {
			return compare(a.Kind, b.Kind)
		}
		return compare(a.Slug, b.Slug)
	})
	return out
}
