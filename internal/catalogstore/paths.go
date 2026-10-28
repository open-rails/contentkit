package catalogstore

import (
	"context"
	"encoding/json"

	entitystore "github.com/open-rails/contentkit/internal/entitystore"
)

// entitySlugs is a path entity's code slugs: the default and per language.
type entitySlugs struct {
	slug  string
	slugs map[string]string
}

func (e entitySlugs) slugFor(language string) string {
	if s := e.slugs[language]; s != "" {
		return s
	}
	return e.slug
}

// decorativePath joins an entity slug and a title slug, skipping empty ones.
func decorativePath(entity, title string) string {
	switch {
	case entity == "":
		return title
	case title == "":
		return entity
	}
	return entity + "/" + title
}

// pathEntities finds each item's path entity: the first effective, active,
// public content-level membership in its kind's PathEntity, by position, then
// slug. Items without one are absent.
func (s *Store) pathEntities(ctx context.Context, q entitystore.Querier, ids []string) (map[string]entitySlugs, error) {
	var kinds, entities []string
	for name, k := range s.cfg.Kinds {
		if k.PathEntity != "" {
			kinds, entities = append(kinds, name), append(entities, k.PathEntity)
		}
	}
	out := map[string]entitySlugs{}
	if len(kinds) == 0 || len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (m.content_id) m.content_id, k.slug, k.slugs::text FROM `+s.t.items+` c
 JOIN unnest($2::text[], $3::text[]) AS p(kind, entity) ON p.kind = c.kind
 JOIN `+s.t.memberships+` m ON m.tenant_id = c.tenant_id AND m.content_id = c.id AND m.version_id IS NULL AND m.state = 'active'
 JOIN `+s.t.entities+` g ON g.tenant_id = m.tenant_id AND g.id = m.entity_id AND g.kind = p.entity AND g.state = 'active' AND NOT g.internal
 JOIN `+s.qs+`.content_codes k ON k.tenant_id = g.tenant_id AND k.content_kind = g.kind AND k.content_id = g.id AND k.merged_into IS NULL
 WHERE c.tenant_id = $1 AND c.id = ANY($4)
 ORDER BY m.content_id, m.position NULLS LAST, k.slug, g.id`, s.tenant, kinds, entities, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		var e entitySlugs
		if err := rows.Scan(&id, &e.slug, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &e.slugs); err != nil {
			return nil, err
		}
		out[id] = e
	}
	return out, rows.Err()
}

// RefreshPaths re-derives the decorative paths of every item whose kind has
// a path entity, in batches; run it after enabling or changing a kind's
// path_entity. Idempotent; it returns the items visited.
func (s *Store) RefreshPaths(ctx context.Context) (int, error) {
	var pathKinds []string
	for name, k := range s.cfg.Kinds {
		if k.PathEntity != "" {
			pathKinds = append(pathKinds, name)
		}
	}
	if len(pathKinds) == 0 {
		return 0, nil
	}
	visited, after := 0, ""
	for {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return visited, err
		}
		// Content rows first, in id order, as catalog writes lock them.
		rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.items+` WHERE tenant_id = $1 AND kind = ANY($2) AND id > $3 ORDER BY id LIMIT 500 FOR UPDATE`,
			s.tenant, pathKinds, after)
		if err != nil {
			_ = tx.Rollback(ctx)
			return visited, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				_ = tx.Rollback(ctx)
				return visited, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			_ = tx.Rollback(ctx)
			return visited, err
		}
		if err := s.putCodes(ctx, tx, ids); err != nil {
			_ = tx.Rollback(ctx)
			return visited, err
		}
		if err := tx.Commit(ctx); err != nil {
			return visited, err
		}
		visited, after = visited+len(ids), ids[len(ids)-1]
	}
}
