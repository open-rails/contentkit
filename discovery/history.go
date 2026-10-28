package discovery

import (
	"time"
)

// HistoryOptions select one page of the signed-in reader's history.
type HistoryOptions struct {
	Reader
	Kind string
	// Status is seen, in_progress or completed; anything they touched by
	// default.
	Status       string
	CardEntities []string
	Cursor       string
	Summary      bool
	// Limit defaults to 24 and is capped at 100.
	Limit int
}

// TypeClick attributes an explicit card selection to its render.
const TypeClick = "click"

// Signals is what a client reports, for the signed-in reader or, signed
// out, for the browser session Session names.
type Signals struct {
	Session string        `json:"session,omitempty"`
	Signals []SignalInput `json:"signals"`
}

// SignalInput is one event on a content item or one of its versions.
type SignalInput struct {
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
	// Only click is accepted here. Measured reading has its own snapshot route.
	Type       string    `json:"type"`
	EventID    string    `json:"event_id"`
	Revision   uint64    `json:"revision,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	// RenderID, Surface and Position attribute it to an exposure.
	RenderID string `json:"render_id,omitempty"`
	Surface  string `json:"surface,omitempty"`
	Position uint32 `json:"position,omitempty"`
	// ActivityToken is the server's immutable access snapshot from /activity-contexts.
	ActivityToken string `json:"activity_token,omitempty"`
}

// Exposures are result lists a client rendered or showed.
type Exposures struct {
	Session   string          `json:"session,omitempty"`
	Exposures []ExposureInput `json:"exposures"`
}

// ExposureInput is one result list at one stage.
type ExposureInput struct {
	// RenderID is stable per render and shared with its clicks.
	RenderID string `json:"render_id"`
	// Stage is served, rendered or visible.
	Stage    string `json:"stage"`
	Revision uint64 `json:"revision,omitempty"`
	QueryID  string `json:"query_id,omitempty"`
	// SearchQuery is normalized like recent search history, at most 200 runes.
	SearchQuery string `json:"search_query,omitempty"`
	Surface     string `json:"surface,omitempty"`
	Ranker      string `json:"ranker,omitempty"`
	Language    string `json:"language,omitempty"`
	// OccurredAt is the render's time; now by default.
	OccurredAt *time.Time  `json:"occurred_at,omitempty"`
	Shown      []Placement `json:"shown"`
}

// Placement is one shown content item at its 1-based position.
type Placement struct {
	ContentID     string `json:"content_id"`
	Position      uint32 `json:"position"`
	VersionID     string `json:"version_id,omitempty"`
	ActivityToken string `json:"activity_token,omitempty"`
}
