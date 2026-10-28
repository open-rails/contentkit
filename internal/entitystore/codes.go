package entitystore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/codes"
)

const entityColumns = `g.id, g.kind, g.slug, g.scope_id, coalesce(g.parent_id, ''), g.state, coalesce(g.merged_into, ''), g.attributes, g.cover,
 coalesce(g.avatar, ''), coalesce(g.owner_user_id, ''), coalesce(g.auth_scope, ''), g.created_at, g.updated_at, g.revision, g.internal`

func scanEntity(row pgx.Row) (entities.Entity, error) {
	var g entities.Entity
	var avatar string
	err := row.Scan(&g.ID, &g.Kind, &g.Slug, &g.ScopeID, &g.ParentID, &g.State, &g.MergedInto, &g.Attributes, &g.Cover,
		&avatar, &g.OwnerUserID, &g.AuthScope, &g.CreatedAt, &g.UpdatedAt, &g.Revision, &g.Internal)
	if err == pgx.ErrNoRows {
		return g, ErrNotFound
	}
	if g.Attributes == nil {
		g.Attributes = map[string]any{}
	}
	if avatar != "" {
		g.Avatar = &entities.Avatar{Image: avatar}
	}
	return g, err
}

func scanEntities(rows pgx.Rows) ([]entities.Entity, error) {
	defer rows.Close()
	out := []entities.Entity{}
	for rows.Next() {
		g, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// syncCodes gives the entities their content codes (contenturl) and slugs them
// from their canonical names: one slug per language, the default in the first
// configured language that has a name.
func (s *Store) syncCodes(ctx context.Context, q Querier, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.kind, g.id,
 coalesce(jsonb_object_agg(m.language, m.name) FILTER (WHERE m.language IS NOT NULL), '{}')::text
 FROM %s g LEFT JOIN %s m ON m.tenant_id = g.tenant_id AND m.entity_id = g.id AND m.kind = 'name'
 WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) GROUP BY g.kind, g.id`,
		s.table("content_entities"), s.table("content_entity_names")), s.tenant, ids)
	if err != nil {
		return err
	}
	var entries []codes.Entry
	for rows.Next() {
		var e codes.Entry
		var raw string
		if err := rows.Scan(&e.Kind, &e.ID, &raw); err != nil {
			rows.Close()
			return err
		}
		var names map[string]string
		if err := json.Unmarshal([]byte(raw), &names); err != nil {
			rows.Close()
			return err
		}
		e.Slugs = make(map[string]string, len(names))
		langs := make([]string, 0, len(names))
		for l, n := range names {
			if _, err := codes.Language(l); err != nil {
				continue // not a URL language segment
			}
			if slug := codes.Slugify(n); slug != "" {
				e.Slugs[l] = slug
				langs = append(langs, l)
			}
		}
		sort.Strings(langs)
		for _, l := range append(append([]string(nil), s.cfg.Languages...), langs...) {
			if slug := e.Slugs[l]; slug != "" {
				e.Slug = slug
				break
			}
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	watched := s.pathEntityIDs(entries)
	before, err := s.codeSlugs(ctx, q, watched)
	if err != nil {
		return err
	}
	if _, err := codes.Put(ctx, codes.Pgx(q), s.qs, s.tenant, entries); err != nil {
		return err
	}
	after, err := s.codeSlugs(ctx, q, watched)
	if err != nil {
		return err
	}
	var renamed []string
	for _, id := range watched {
		if before[id] != after[id] {
			renamed = append(renamed, id)
		}
	}
	return s.refreshMembers(ctx, q, renamed)
}

// pathEntityIDs are the entries whose kind some content kind's decorative
// path names (kinds.Kind.PathEntity).
func (s *Store) pathEntityIDs(entries []codes.Entry) []string {
	var out []string
	for _, e := range entries {
		for _, k := range s.cfg.Kinds {
			if k.PathEntity == e.Kind {
				out = append(out, e.ID)
				break
			}
		}
	}
	return out
}

// codeSlugs snapshots the entities' code slugs, default and localized.
func (s *Store) codeSlugs(ctx context.Context, q Querier, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT content_id, slug || ' ' || slugs::text FROM `+s.qs+`.content_codes WHERE tenant_id = $1 AND content_id = ANY($2::text[])`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// refreshMembers re-derives the facets of the entities' direct content members,
// so the engine follows a renamed path entity into their paths.
func (s *Store) refreshMembers(ctx context.Context, q Querier, entityIDs []string) error {
	if len(entityIDs) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT content_id FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[]) AND version_id IS NULL`,
		s.table("content_memberships")), s.tenant, entityIDs)
	if err != nil {
		return err
	}
	contentIDs, err := collectStrings(rows)
	if err != nil {
		return err
	}
	return s.refreshFacets(ctx, q, contentIDs)
}
