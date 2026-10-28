package entities

import (
	"fmt"
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
