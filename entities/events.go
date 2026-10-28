package entities

import "context"

// Entity event types. The engine appends them to the catalog event log
// (GET /v1/events) in the mutation's transaction (Options.EntitiesChanged).
const (
	EventCreated  = "entity.created"
	EventUpdated  = "entity.updated"
	EventDeleted  = "entity.deleted"
	EventRestored = "entity.restored"
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

func (s *Store) record(ctx context.Context, q Querier, changes []Change) error {
	if s.recorded == nil || len(changes) == 0 {
		return nil
	}
	return s.recorded(ctx, q, changes)
}

func changesOf(typ string, list []Entity) []Change {
	out := make([]Change, len(list))
	for i, g := range list {
		out[i] = Change{Type: typ, Kind: g.Kind, ID: g.ID}
	}
	return out
}

// transition is the event for an entity whose state moved from before to after.
func transition(before, after State) string {
	switch {
	case before != StateDeleted && after == StateDeleted:
		return EventDeleted
	case before == StateDeleted && after == StateActive:
		return EventRestored
	}
	return EventUpdated
}
