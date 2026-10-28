package catalog

import (
	"time"
)

// PublishInput publishes now, or at PublishAt when it is in the future.
type PublishInput struct {
	PublishAt *time.Time `json:"publish_at,omitempty"`
}

// CreatorsInput replaces the creators.
type CreatorsInput struct {
	Creators []Creator `json:"creators"`
}

// OrderInput orders versions.
type OrderInput struct {
	IDs []string `json:"ids"`
}

// SegmentsInput replaces a version's segments.
type SegmentsInput struct {
	Segments []SegmentInput `json:"segments"`
}

// InlineImage is an inline image of a body: Ref goes where its URL would
// ("contentkit:i-{uuid}"); URL shows it to the editor now (null until it has
// a file to show).
type InlineImage struct {
	Ref string  `json:"ref"`
	URL *string `json:"url"`
}

// ManageQuery selects content items to manage.
type ManageQuery struct {
	Kind string
	// State is draft, scheduled, published (live) or deleted; "" is every
	// state but deleted.
	State   string
	Channel string
	// Text matches title, description or body, ignoring case.
	Text string
	// Code selects the item with this content code; with State "" it matches
	// deleted items too, so editor links keep resolving.
	Code string
	// IDs selects these items (at most MaxManageIDs); with State "" it
	// matches deleted items too, so member lists resolve every member.
	IDs []string
	// Access keeps items whose own rule has this level; Tier those whose rule
	// names this configured tier (implying the tier level).
	Access, Tier  string
	Limit, Offset int
}

// MaxManageIDs bounds ManageQuery.IDs.
const MaxManageIDs = 100

// ManageCount is how many items a manage listing selects.
type ManageCount struct {
	Total int `json:"total"`
	// Truncated makes Total a lower bound.
	Truncated bool `json:"truncated,omitempty"`
}
