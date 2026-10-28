package discovery

import (
	"time"

	"github.com/open-rails/contentkit/channels"
)

const (
	TargetContents = "contents"
)

const (
	TargetChannels = "channels"
)

// MineOptions select one page of the signed-in reader's favorites.
type MineOptions struct {
	Reader
	// Target is contents (the default) or channels.
	Target string
	// Kind is one content kind; every kind by default.
	Kind         string
	CardEntities []string
	Cursor       string
	// Limit defaults to 24 and is capped at 100.
	Limit int
	// Summary adds counts scoped to this reader's matching contents, before pagination.
	Summary bool
}

// Interaction is one content item or channel the reader favorited or reacted
// to, as its card shows it now.
type Interaction struct {
	Channel *channels.Card `json:"channel,omitempty"`
	Content *Card          `json:"content,omitempty"`
	// Value is 1 for a favorite.
	Value int16 `json:"value"`
	// At is when the reader favorited or last reacted.
	At time.Time `json:"at"`
}

// InteractionPage is one page of the reader's favorites, most
// recent first.
type InteractionPage struct {
	Items []Interaction `json:"items"`
	// NextCursor pages on; empty on the last page.
	NextCursor string          `json:"next_cursor,omitempty"`
	Summary    *ListingSummary `json:"summary,omitempty"`
}
