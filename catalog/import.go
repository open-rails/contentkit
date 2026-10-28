package catalog

import (
	"time"

	"github.com/open-rails/contentkit/access"
)

// ImportContent is a content item as the bulk import writes it: its id,
// state and times kept, trusted (not screened). Nil Creators, Text and a
// version's Text keep what is stored; given ones replace it. Chapters are
// authored through PutVersions after content and source media are imported.
type ImportContent struct {
	ID          string
	Kind        string
	State       string
	PublishAt   *time.Time
	PublishedAt *time.Time
	DeletedAt   *time.Time
	// Comments is open (when empty), closed or hidden.
	Comments     string
	Access       *access.Rule
	Restrictions []string
	Attributes   map[string]any
	ChannelID    string
	Creators     []Creator
	Versions     []ImportVersion
	Code         string
	CreatedAt    *time.Time
	UpdatedAt    *time.Time
}

// ImportVersion is one version of an ImportContent, positioned as listed.
type ImportVersion struct {
	ID string
	// Code is the version code to keep; empty keeps the stored one or
	// assigns one. A version never changes its code.
	Code        string
	Languages   []string
	Traits      []string
	Label       string
	State       string
	PublishAt   *time.Time
	PublishedAt *time.Time
	DeletedAt   *time.Time
	Access      *access.Rule
	Attributes  map[string]any
	Ordinal     *int
	// Cover selects one of the version's cover uploads (i-{uuid}).
	Cover            string
	OriginalTitle    *string
	OriginalLanguage *string
	Text             map[string]VersionText
	// Segments must be nil. Use PutVersions after importing source media so
	// chapter changes hold the recovered manifest lock through commit.
	Segments  []Segment
	CreatedAt *time.Time
	UpdatedAt *time.Time
}
