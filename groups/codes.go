package groups

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/codes"
)

const groupColumns = `g.id, g.kind, g.slug, g.scope_id, coalesce(g.parent_id, ''), g.state, coalesce(g.merged_into, ''), g.attributes, g.cover,
 coalesce(g.avatar, ''), coalesce(g.owner_user_id, ''), coalesce(g.auth_scope, ''), g.created_at, g.updated_at, g.revision`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	var avatar string
	err := row.Scan(&g.ID, &g.Kind, &g.Slug, &g.ScopeID, &g.ParentID, &g.State, &g.MergedInto, &g.Attributes, &g.Cover,
		&avatar, &g.OwnerUserID, &g.AuthScope, &g.CreatedAt, &g.UpdatedAt, &g.Revision)
	if err == pgx.ErrNoRows {
		return g, ErrNotFound
	}
	if g.Attributes == nil {
		g.Attributes = map[string]any{}
	}
	if avatar != "" {
		g.Avatar = &Avatar{Image: avatar}
	}
	return g, err
}

func scanGroups(rows pgx.Rows) ([]Group, error) {
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// syncCodes gives the groups their content codes (contenturl) and slugs them
// from their canonical names: one slug per language, the default in the first
// configured language that has a name.
func (s *Store) syncCodes(ctx context.Context, q Querier, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.kind, g.id,
 coalesce(jsonb_object_agg(m.language, m.name) FILTER (WHERE m.language IS NOT NULL), '{}')::text
 FROM %s g LEFT JOIN %s m ON m.tenant_id = g.tenant_id AND m.group_id = g.id AND m.kind = 'name'
 WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) GROUP BY g.kind, g.id`,
		s.table("content_groups"), s.table("content_group_names")), s.tenant, ids)
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
	_, err = codes.Put(ctx, codes.Pgx(q), s.qs, s.tenant, entries)
	return err
}
