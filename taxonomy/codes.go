package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/open-rails/contentkit/internal/codes"
)

type codesQuerier struct{ q querier }

func (c codesQuerier) Query(ctx context.Context, sql string, args ...any) (codes.Rows, error) {
	return c.q.Query(ctx, sql, args...)
}

// syncCodes gives the nodes their content codes (contenturl) and slugs them
// from their canonical names: one slug per language, the default in the first
// configured language that has a name.
func (s *Store) syncCodes(ctx context.Context, q querier, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT n.kind, n.taxonomy_id,
 coalesce(jsonb_object_agg(m.language, m.name) FILTER (WHERE m.language IS NOT NULL), '{}')::text
 FROM %s n LEFT JOIN %s m ON m.tenant_id = n.tenant_id AND m.taxonomy_id = n.taxonomy_id AND m.kind = 'name'
 WHERE n.tenant_id = $1 AND n.taxonomy_id = ANY($2::text[]) GROUP BY n.kind, n.taxonomy_id`,
		s.table("content_nodes"), s.table("content_node_names")), s.tenant, ids)
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
		for _, l := range append(append([]string(nil), s.languages...), langs...) {
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
	_, err = codes.Put(ctx, codesQuerier{q}, s.qs, s.tenant, entries)
	return err
}
