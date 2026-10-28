package entitystore

import (
	"context"
	"fmt"
	"strings"

	entities "github.com/open-rails/contentkit/entities"
)

// MatchNames resolves names to entity ids, one NameMatch per name in order. It
// neither creates entities nor reserves names; merged and deleted entities
// never match.
func (s *Store) MatchNames(ctx context.Context, m entities.MatchQuery) ([]entities.NameMatch, error) {
	if _, err := s.entityKind(m.Kind); err != nil {
		return nil, err
	}
	if m.Scope != "" {
		if err := validateID(m.Scope); err != nil {
			return nil, err
		}
	}
	language := ""
	if strings.TrimSpace(m.Language) != "" {
		var err error
		if language, err = normalizeLanguage(m.Language); err != nil {
			return nil, err
		}
	}
	if len(m.Names) == 0 || len(m.Names) > entities.MaxMatchNames {
		return nil, fmt.Errorf("%w: match 1 to %d names", entities.ErrInvalid, entities.MaxMatchNames)
	}
	for _, name := range m.Names {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: a name is empty", entities.ErrInvalid)
		}
	}
	literal := `AND n.name COLLATE "C" = q.name COLLATE "C"`
	if m.Normalized {
		literal = ""
	}
	out := make([]entities.NameMatch, len(m.Names))
	for i, name := range m.Names {
		out[i] = entities.NameMatch{Name: name, IDs: []entities.EntityID{}}
	}
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT q.n, m.id FROM unnest($5::text[]) WITH ORDINALITY AS q(name, n)
 CROSS JOIN LATERAL (SELECT DISTINCT g.id FROM %[1]s n JOIN %[2]s g ON g.tenant_id = n.tenant_id AND g.id = n.entity_id
  WHERE n.tenant_id = $1 AND n.normalized = %[3]s.contentkit_keyword_normalize(q.name) %[4]s AND ($4 = '' OR n.language = $4)
  AND g.kind = $2 AND g.scope_id = $3 AND g.state = 'active' AND NOT g.internal AND %[5]s
  ORDER BY g.id LIMIT 2) m
 ORDER BY q.n, m.id`, s.table("content_entity_names"), s.table("content_entities"), s.qs, literal, s.domainPredicate("g")),
			s.tenant, m.Kind, string(m.Scope), language, m.Names)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n int
			var id entities.EntityID
			if err := rows.Scan(&n, &id); err != nil {
				return err
			}
			out[n-1].IDs = append(out[n-1].IDs, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
