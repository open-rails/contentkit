package catalog

import (
	"encoding/json"
	"time"
)

// DefaultEventRetention is independent of readers' progress.
const DefaultEventRetention = 90 * 24 * time.Hour

// EventQuery reads one app's retained log. Callers own their continuation.
type EventQuery struct {
	After string
	Limit int
	Types []string
}

// EventRecord is an immutable committed catalog fact. IDs are decimal strings
// so JSON clients do not lose bigint precision.
type EventRecord struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	Type      string `json:"type"`
	Kind      string `json:"kind"`
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
	// EntityID is an entity event's subject; MergedInto its survivor.
	EntityID   string          `json:"entity_id,omitempty"`
	MergedInto string          `json:"merged_into,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
	RecordedAt time.Time       `json:"recorded_at"`
	Source     string          `json:"source"`
	Scope      string          `json:"scope"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// EventPage always provides a continuation, including an empty stream.
type EventPage struct {
	Events           []EventRecord `json:"events"`
	NextCursor       string        `json:"next_cursor"`
	HasMore          bool          `json:"has_more"`
	RetentionSeconds int64         `json:"retention_seconds"`
}
