package entities

import (
	"time"
)

// Sort orders an entity listing; every order is made total by id.
type Sort string

const (
	// SortName orders by sort key, else display name. The default.
	SortName Sort = "name"
)

const (
	// SortCount orders by content count, largest first.
	SortCount Sort = "count"
)

const (
	// SortNewest orders by creation, newest first.
	SortNewest Sort = "newest"
)

// MaxListIDs bounds ListOptions.IDs.
const MaxListIDs = 100

// EntityCard is a listed entity: what a directory page or a chip needs.
type EntityCard struct {
	ID       EntityID `json:"id"`
	Kind     string   `json:"kind"`
	Slug     string   `json:"slug"`
	Internal bool     `json:"internal"`
	ScopeID  EntityID `json:"scope_id,omitempty"`
	ParentID EntityID `json:"parent_id,omitempty"`
	State    State    `json:"state"`
	// MergedInto is a merged entity's final survivor.
	MergedInto   EntityID `json:"merged_into,omitempty"`
	Name         string   `json:"name"`
	NameLanguage string   `json:"name_language,omitempty"`
	Code         string   `json:"code,omitempty"`
	Path         string   `json:"path,omitempty"`
	Cover        *Cover   `json:"cover,omitempty"`
	// Avatar is a user-owned entity's avatar.
	Avatar     *Avatar        `json:"avatar,omitempty"`
	Count      int            `json:"count"`
	Attributes map[string]any `json:"attributes"`
	// Access is an entitlement-backed entity's key and the
	// viewer's standing.
	Access    *EntityAccess `json:"access,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// EntityPage is one page of a listing.
type EntityPage struct {
	Entities []EntityCard `json:"entities"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// Total is every matching entity, on numbered pages only.
	Total *int `json:"total,omitempty"`
}

// EntityDetail is one entity with everything its page shows.
type EntityDetail struct {
	Entity
	Name         string       `json:"name"`
	NameLanguage string       `json:"name_language,omitempty"`
	Code         string       `json:"code,omitempty"`
	Path         string       `json:"path,omitempty"`
	Names        []Name       `json:"names"`
	Text         []Text       `json:"text"`
	Parent       *EntityCard  `json:"parent,omitempty"`
	Scope        *EntityCard  `json:"scope,omitempty"`
	Children     []EntityCard `json:"children"`
	Counts       []Count      `json:"counts"`
	// Access is an entitlement-backed entity's key and the
	// viewer's standing.
	Access *EntityAccess `json:"access,omitempty"`
}
