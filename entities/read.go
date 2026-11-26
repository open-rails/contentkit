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

const (
	// SortPopular orders one kind by its popularity on the request's site
	// over a window: the sum of its contents' (see the rankings), the
	// entities without activity in the window by count, then the ones
	// whose readers were mostly dissatisfied. The default of a public
	// listing of one kind.
	SortPopular Sort = "popular"
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
	// Popularity is the entity's standing in a popular listing's window;
	// absent elsewhere and for an entity without activity in it.
	Popularity *EntityPopularity `json:"popularity,omitempty"`
}

// EntityPopularity is an entity's standing over a ranking window.
type EntityPopularity struct {
	// Score is the sum of its contents' popularity: quality-weighted
	// person-days, per live day for the week.
	Score float64 `json:"score"`
	// Polarization, from 0 to 1, is how split its readers are between
	// strong positives and strong negatives.
	Polarization float64 `json:"polarization"`
	// ReadThrough is an artist's read-through rate; absent for other kinds
	// and before it can be measured.
	ReadThrough *ReadThrough `json:"read_through,omitempty"`
}

// ReadThrough is the share of an artist's readers who went on to read
// another of its works within 30 days of their read.
type ReadThrough struct {
	Rate    float64 `json:"rate"`
	Readers int     `json:"readers"`
}

// EntityPage is one page of a listing.
type EntityPage struct {
	Entities []EntityCard `json:"entities"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// Total is every matching entity, on numbered pages only.
	Total *int `json:"total,omitempty"`
	// Popularity is the ranking a popular listing served.
	Popularity *PopularWindow `json:"popularity,omitempty"`
}

// PopularWindow is the ranking a popular list served.
type PopularWindow struct {
	// Window is the ranking window as requested, canonical: week, 30d, 90d,
	// year, all, month:YYYY-MM, year:YYYY or a range of days.
	Window string `json:"window"`
	// From and Until are its UTC days [From, Until).
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
	// ComputedAt is when it was ranked; absent while it is not Ready.
	ComputedAt *time.Time `json:"computed_at,omitempty"`
	// Ready is false while the window has not been ranked: the list is in
	// the order of entries without activity.
	Ready bool `json:"ready"`
	// Stale is a featured ranking whose hourly refresh has since failed.
	Stale bool `json:"stale,omitempty"`
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
