package entities

import (
	"slices"
	"strings"
	"time"
)

// ImportEntity is an entity as the bulk import writes it: its id kept. Nil
// Names and Text keep what is stored; given ones replace it.
type ImportEntity struct {
	ID   EntityID
	Kind string
	Slug string
	// Internal preserves the stored classification when omitted.
	Internal    *bool
	ScopeID     EntityID
	ParentID    EntityID
	State       State
	Attributes  map[string]any
	Cover       *Cover
	Avatar      *Avatar
	OwnerUserID string
	// AuthScope names the entity's AuthKit scope; empty keeps the stored one.
	AuthScope string
	Names     []Name
	Text      []Text
	Code      string
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// sameNames reports whether given names, duplicates aside, are the stored
// ones.
func sameNames(given []nameRow, stored []Name) bool {
	type name struct{ language, kind, name string }
	var want, have []name
	for _, r := range given {
		want = append(want, name{r.Language, r.Kind, r.Name})
	}
	for _, n := range stored {
		have = append(have, name{n.Language, string(n.Kind), n.Name})
	}
	cmp := func(a, b name) int {
		return strings.Compare(a.language+"\x00"+a.kind+"\x00"+a.name, b.language+"\x00"+b.kind+"\x00"+b.name)
	}
	slices.SortFunc(want, cmp)
	slices.SortFunc(have, cmp)
	return slices.Equal(slices.Compact(want), have)
}
