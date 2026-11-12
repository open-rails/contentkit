package entitystore

import (
	"context"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5"

	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// LiveListed reads completed-publication facets. live_from is populated only
// after durable publication; a scheduled date alone never creates availability.
// Counts and automatic covers follow committed edits without projection jobs.
const LiveListed = `NOT f.deleted AND f.live_from IS NOT NULL AND f.live_from <= now()`

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

func (s *Store) counts(ctx context.Context, q Querier, ids []string) (map[entities.EntityID][]entities.Count, error) {
	boundary, args := s.boundary.SQL("f", "entity_domain")
	params := pgx.NamedArgs{"tenant": s.tenant, "ids": ids}
	maps.Copy(params, args)
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.id, f.kind, l.language, count(DISTINCT f.content_id)::int
 FROM %s g JOIN %s f ON f.tenant_id=g.tenant_id
  AND (f.entity_ids @> ARRAY[g.id] OR f.owner_entity_id=g.id)
 CROSS JOIN LATERAL unnest(array_append(f.languages, '')) l(language)
 WHERE g.tenant_id=@tenant AND g.id=ANY(@ids::text[]) AND g.state='active' AND %s AND %s
 GROUP BY g.id,f.kind,l.language ORDER BY g.id,f.kind,l.language`,
		s.table("content_entities"), s.table("content_facets"), LiveListed, boundary), params)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[entities.EntityID][]entities.Count{}
	for rows.Next() {
		var id string
		var c entities.Count
		if err := rows.Scan(&id, &c.ContentKind, &c.Language, &c.Count); err != nil {
			return nil, err
		}
		out[entities.EntityID(id)] = append(out[entities.EntityID(id)], c)
	}
	return out, rows.Err()
}
