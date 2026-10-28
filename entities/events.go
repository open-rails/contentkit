package entities

const (
	EventCreated = "entity.created"
)

const (
	EventUpdated = "entity.updated"
)

const (
	EventDeleted = "entity.deleted"
)

const (
	EventRestored = "entity.restored"
)

const (
	// EventMerged names the final survivor in MergedInto. Earlier merges into
	// the merged entity are re-recorded with the same survivor.
	EventMerged = "entity.merged"
)

// Change is one committed entity mutation.
type Change struct {
	Type       string
	Kind       string
	ID         EntityID
	MergedInto EntityID
}
