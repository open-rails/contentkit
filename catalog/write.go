package catalog

import (
	"time"

	"github.com/open-rails/contentkit/access"
)

// ContentInput creates a content item, a draft until published.
type ContentInput struct {
	Kind string `json:"kind"`
	// Comments is open (the default when permitted), closed (existing ones
	// show, new ones are refused) or hidden. A kind excluding open defaults to
	// its first configured comment mode.
	Comments string `json:"comments,omitempty"`
	// Access defaults to public when permitted, otherwise the kind's first
	// allowed level (and the first configured tier for tier access).
	Access       *access.Rule   `json:"access,omitempty"`
	Restrictions []string       `json:"restrictions,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	// ChannelID is the primary channel; its current editors govern the item.
	// It never changes.
	ChannelID string `json:"channel_id,omitempty"`
	// Creators nil credits the creating person as its author.
	Creators *[]Creator `json:"creators,omitempty"`
	// Versions defines initial editions. Omission creates one empty edition.
	// A kind with hidden versions accepts at most one initial edition.
	Versions []VersionInput `json:"versions,omitempty"`
}

// ContentPatch edits a content item; nil fields stay.
type ContentPatch struct {
	Comments     *string        `json:"comments,omitempty"`
	Access       *access.Rule   `json:"access,omitempty"`
	Restrictions *[]string      `json:"restrictions,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"` // replaces all
	Creators     *[]Creator     `json:"creators,omitempty"`
	// Revision, when set, refuses the edit (409) unless it is current.
	Revision *int64 `json:"revision,omitempty"`
}

// VersionInput creates or edits a version; nil fields stay (or default).
type VersionInput struct {
	Languages []string  `json:"languages,omitempty"`
	Traits    *[]string `json:"traits,omitempty"`
	Label     *string   `json:"label,omitempty"`
	// State is draft or published intent. A version becomes live only when its
	// durable publication completes. A version created with its gallery
	// defaults to published intent, one added later to draft.
	State     string     `json:"state,omitempty"`
	PublishAt *time.Time `json:"publish_at,omitempty"`
	// Access is this edition's rule. ClearAccess copies the current gallery
	// creation default once; subsequent gallery edits never change it.
	Access      *access.Rule           `json:"access,omitempty"`
	ClearAccess bool                   `json:"clear_access,omitempty"`
	Attributes  map[string]any         `json:"attributes,omitempty"` // replaces all
	Text        map[string]VersionText `json:"text,omitempty"`       // replaces all
	Segments    *[]SegmentInput        `json:"segments,omitempty"`   // replaces all
	Ordinal     *int                   `json:"ordinal,omitempty"`
	// Cover selects one of the version's cover uploads by name (i-{uuid});
	// "" returns to its first.
	Cover            *string `json:"cover,omitempty"`
	OriginalTitle    *string `json:"original_title,omitempty"`
	OriginalLanguage *string `json:"original_language,omitempty"`
}

// Update is one ContentPatch.
type Update struct {
	ID string
	ContentPatch
}

// Publish publishes a content item now, or at At when it is in the future
// (scheduled).
type Publish struct {
	ContentID string
	At        *time.Time
}

// VersionPut creates (VersionID "") or edits a version.
type VersionPut struct {
	ContentID string
	VersionID string
	VersionInput
}

// VersionKey names one version.
type VersionKey struct{ ContentID, VersionID string }
