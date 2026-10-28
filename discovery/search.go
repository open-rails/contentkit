package discovery

import (
	"github.com/open-rails/contentkit/entities"
)

// SortRelevance orders search results by match, personalized for a signed-in
// reader.
const SortRelevance = "relevance"

// SearchPage is a page of search results. The first page also carries the
// entities matching the query that have contents the reader can see.
type SearchPage struct {
	ContentPage
	Entities []entities.EntityCard `json:"entities,omitempty"`
}

// TypeaheadOptions select suggestions while a reader types.
type TypeaheadOptions struct {
	Reader
	Query string
	// Kind limits content suggestions to one kind.
	Kind string
	// EntityKinds are the entity kinds suggested; every kind with a page by
	// default.
	EntityKinds []string
	// Limit is per section: 8 by default, at most 20.
	Limit int
}

// Typeahead is the suggestions: content cards and entities, best first.
type Typeahead struct {
	Contents []Card                `json:"contents"`
	Entities []entities.EntityCard `json:"entities"`
}
