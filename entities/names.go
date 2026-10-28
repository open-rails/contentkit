package entities

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// EntityNames is one entity's names in a names write.
type EntityNames struct {
	ID    EntityID `json:"id"`
	Names []Name   `json:"names"`
}

type nameRow struct {
	Ordinal  int    `json:"ordinal"`
	EntityID string `json:"entity_id"`
	Language string `json:"language"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

func validateNames(id EntityID, names []Name) ([]nameRow, error) {
	rows := make([]nameRow, 0, len(names))
	canonical := map[string]string{}
	for _, n := range names {
		lang, err := normalizeLanguage(n.Language)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(n.Name)
		if name == "" || utf8.RuneCountInString(name) > 512 {
			return nil, fmt.Errorf("%w: name %q must be 1-512 characters", ErrInvalid, name)
		}
		kind := n.Kind
		if kind == "" {
			kind = NameAlias
		}
		if kind != NameCanonical && kind != NameAlias {
			return nil, fmt.Errorf("%w: name kind %q", ErrInvalid, kind)
		}
		if kind == NameCanonical {
			if prev, dup := canonical[lang]; dup && prev != name {
				return nil, fmt.Errorf("%w: two canonical names for %s: %q and %q", ErrInvalid, lang, prev, name)
			}
			canonical[lang] = name
		}
		rows = append(rows, nameRow{Ordinal: len(rows), EntityID: string(id), Language: lang, Kind: string(kind), Name: name})
	}
	return rows, nil
}

// insertNames upserts names. A new canonical name demotes the previous one of
// its language to an alias, so a rename keeps the old name searchable.
func (s *Store) insertNames(ctx context.Context, q Querier, rows []nameRow) error {
	if len(rows) == 0 {
		return nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	const recordset = `jsonb_to_recordset($2::jsonb) AS r(ordinal int, entity_id text, language text, kind text, name text)`
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s n SET kind = 'alias' FROM %s
 WHERE r.kind = 'name' AND n.tenant_id = $1 AND n.entity_id = r.entity_id AND n.language = r.language AND n.kind = 'name'
   AND n.normalized <> %s.contentkit_keyword_normalize(r.name)`, s.table("content_entity_names"), recordset, s.qs), s.tenant, data); err != nil {
		return err
	}
	// Equal normalized forms collapse to one row; a canonical name wins over
	// an alias, and the last input of the same kind wins.
	_, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s AS existing (tenant_id, entity_id, language, kind, name)
 SELECT DISTINCT ON (r.entity_id, r.language, %[2]s.contentkit_keyword_normalize(r.name)) $1, r.entity_id, r.language, r.kind, r.name
 FROM %[3]s ORDER BY r.entity_id, r.language, %[2]s.contentkit_keyword_normalize(r.name), (r.kind = 'name') DESC, r.ordinal DESC
 ON CONFLICT (tenant_id, entity_id, language, normalized) DO UPDATE SET kind = EXCLUDED.kind, name = EXCLUDED.name
 WHERE existing.kind <> 'name' OR EXCLUDED.kind = 'name'`, s.table("content_entity_names"), s.qs, recordset), s.tenant, data)
	return err
}

func (s *Store) names(ctx context.Context, q Querier, ids []string) (map[EntityID][]Name, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT entity_id, language, kind, name FROM %s
 WHERE tenant_id = $1 AND entity_id = ANY($2::text[]) ORDER BY entity_id, language, kind DESC, normalized`, s.table("content_entity_names")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[EntityID][]Name{}
	for rows.Next() {
		var id string
		var n Name
		if err := rows.Scan(&id, &n.Language, &n.Kind, &n.Name); err != nil {
			return nil, err
		}
		out[EntityID(id)] = append(out[EntityID(id)], n)
	}
	return out, rows.Err()
}

type nameOp int

const (
	namesSet nameOp = iota
	namesAdd
	namesRemove
)

// SetNames replaces every name and alias of each entity.
func (s *Store) SetNames(ctx context.Context, edits []EntityNames) error {
	return s.editNames(ctx, namesSet, edits)
}

// AddNames upserts names and aliases; a new canonical name demotes the
// previous one of its language to an alias.
func (s *Store) AddNames(ctx context.Context, edits []EntityNames) error {
	return s.editNames(ctx, namesAdd, edits)
}

// RemoveNames deletes the given names, matched by language and normalized
// form; unknown names are ignored.
func (s *Store) RemoveNames(ctx context.Context, edits []EntityNames) error {
	return s.editNames(ctx, namesRemove, edits)
}

func (s *Store) editNames(ctx context.Context, op nameOp, edits []EntityNames) error {
	if len(edits) == 0 {
		return nil
	}
	var rows []nameRow
	ids := make([]EntityID, 0, len(edits))
	for _, e := range edits {
		if err := validateID(e.ID); err != nil {
			return err
		}
		r, err := validateNames(e.ID, e.Names)
		if err != nil {
			return err
		}
		for _, row := range r {
			row.Ordinal = len(rows)
			rows = append(rows, row)
		}
		ids = append(ids, e.ID)
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q Querier) error {
		ents, err := s.lock(ctx, q, list)
		if err != nil {
			return err
		}
		for _, g := range ents {
			if g.State == StateMerged {
				return fmt.Errorf("%w: group %s is merged", ErrConflict, g.ID)
			}
		}
		switch op {
		case namesSet:
			if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`, s.table("content_entity_names")), s.tenant, list); err != nil {
				return err
			}
			if err := s.insertNames(ctx, q, rows); err != nil {
				return err
			}
		case namesAdd:
			if err := s.insertNames(ctx, q, rows); err != nil {
				return err
			}
		case namesRemove:
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s n USING jsonb_to_recordset($2::jsonb) AS r(entity_id text, language text, name text)
 WHERE n.tenant_id = $1 AND n.entity_id = r.entity_id AND n.language = r.language AND n.normalized = %s.contentkit_keyword_normalize(r.name)`,
				s.table("content_entity_names"), s.qs), s.tenant, data); err != nil {
				return err
			}
		}
		return s.touch(ctx, q, ents)
	})
}

// touch bumps the entities' revision, re-slugs their codes from their names and
// queues their documents.
func (s *Store) touch(ctx context.Context, q Querier, ents []Entity) error {
	ids := make([]string, len(ents))
	for i, g := range ents {
		ids[i] = string(g.ID)
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET updated_at = now(), revision = revision + 1 WHERE tenant_id = $1 AND id = ANY($2::text[])`,
		s.table("content_entities")), s.tenant, ids); err != nil {
		return err
	}
	if err := s.syncCodes(ctx, q, ids); err != nil {
		return err
	}
	if err := s.namesChanged(ctx, q, ids); err != nil {
		return err
	}
	if err := s.markDirty(ctx, q, ents); err != nil {
		return err
	}
	return s.record(ctx, q, changesOf(EventUpdated, ents))
}

// namesChanged tells Options.NamesChanged that the entities' names or slugs
// changed.
func (s *Store) namesChanged(ctx context.Context, q Querier, ids []string) error {
	if s.renamed == nil || len(ids) == 0 {
		return nil
	}
	return s.renamed(ctx, q, ids)
}

// lock takes the entities' row locks in id order, so concurrent writers never
// deadlock; a missing entity is ErrNotFound.
func (s *Store) lock(ctx context.Context, q Querier, ids []string) ([]Entity, error) {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) AND %s ORDER BY g.id FOR UPDATE`,
		entityColumns, s.table("content_entities"), s.domainPredicate("g")), s.tenant, sorted)
	if err != nil {
		return nil, err
	}
	found, err := scanEntities(rows)
	if err != nil {
		return nil, err
	}
	if len(found) != len(sorted) {
		return nil, fmt.Errorf("%w: %d of %d groups", ErrNotFound, len(sorted)-len(found), len(sorted))
	}
	return found, nil
}
