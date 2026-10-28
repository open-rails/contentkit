package entities

// MatchQuery selects the entities MatchNames compares names against: active,
// public entities of Kind within Scope.
type MatchQuery struct {
	Kind string
	// Scope is the entity the names are unique within; empty matches unscoped
	// entities only.
	Scope EntityID
	// Language matches names in it only; empty matches every language.
	Language string
	// Normalized compares ContentKit's normalized forms, folding case, accents
	// and whitespace; otherwise names must be equal byte for byte.
	Normalized bool
	// Names are matched whole: a comma never separates two names.
	Names []string
}

// NameMatch is the entities whose canonical name or alias matches Name, in id
// order: none is unresolved, two (at most) is ambiguous.
type NameMatch struct {
	Name string     `json:"name"`
	IDs  []EntityID `json:"ids"`
}

// MaxMatchNames bounds MatchQuery.Names.
const MaxMatchNames = 100
