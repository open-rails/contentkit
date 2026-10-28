package catalog

import (
	"time"

	"github.com/open-rails/contentkit/access"
)

const (
	EventContentCreated = "content.created"
)

const (
	EventContentUpdated = "content.updated"
)

const (
	EventContentScheduled = "content.scheduled"
)

const (
	EventContentPublished = "content.published"
)

const (
	EventContentUnpublished = "content.unpublished"
)

const (
	EventContentDeleted = "content.deleted"
)

const (
	EventContentRestored = "content.restored"
)

const (
	EventContentPurged = "content.purged"
)

const (
	EventVersionCreated = "version.created"
)

const (
	EventVersionUpdated = "version.updated"
)

const (
	EventVersionPublished = "version.published"
)

const (
	EventVersionDeleted = "version.deleted"
)

const (
	EventVersionPurged = "version.purged"
)

const (
	EventAudienceReleased = "version.audience_released"
)

const (
	EventPublicationCompleted = "version.publication_completed"
)

// Event is the catalog fact appended by a mutation. Readers receive EventRecord.
type Event struct {
	ChannelID string
	Type      string
	// Kind is the content's kind, or an entity event's entity kind.
	Kind      string
	ContentID string
	VersionID string
	// EntityID is an entity event's subject; MergedInto its survivor.
	EntityID      string
	MergedInto    string
	OccurredAt    time.Time
	Release       *access.Release
	PublicationID string
	ChapterIDs    []string
	Released      *bool
}
