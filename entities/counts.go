package entities

import (
	"context"
	"fmt"

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

// liveListed reads completed-publication facets. live_from is populated only
// after durable publication; a scheduled date alone never creates availability.
// Counts and automatic covers follow committed edits without projection jobs.
const liveListed = `NOT f.deleted AND NOT f.unplayable AND f.live_from IS NOT NULL AND f.live_from <= now()`

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
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.id, f.kind, l.language, count(DISTINCT f.content_id)::int
 FROM %s g JOIN %s f ON f.tenant_id=g.tenant_id
  AND (f.entity_ids @> ARRAY[g.id] OR f.owner_entity_id=g.id)
 CROSS JOIN LATERAL unnest(array_append(f.languages, '')) l(language)
 WHERE g.tenant_id=$1 AND g.id=ANY($2::text[]) AND g.state='active' AND %s
 GROUP BY g.id,f.kind,l.language ORDER BY g.id,f.kind,l.language`,
		s.table("content_entities"), s.table("content_facets"), liveListed), s.tenant, ids)
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
