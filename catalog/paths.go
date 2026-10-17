package catalog

import (
	"context"
	"encoding/json"

	"github.com/open-rails/contentkit/groups"
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
func (s *Store) pathEntities(ctx context.Context, q groups.Querier, ids []string) (map[string]entitySlugs, error) {
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
  AND (m.active_from IS NULL OR m.active_from <= now()) AND (m.active_until IS NULL OR m.active_until > now())
 JOIN `+s.t.groups+` g ON g.tenant_id = m.tenant_id AND g.id = m.group_id AND g.kind = p.entity AND g.state = 'active' AND NOT g.internal
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
