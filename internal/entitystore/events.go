package entitystore

import (
	"context"

	entities "github.com/open-rails/contentkit/entities"
)

func (s *Store) record(ctx context.Context, q Querier, changes []entities.Change) error {
	if s.recorded == nil || len(changes) == 0 {
		return nil
	}
	return s.recorded(ctx, q, changes)
}

func changesOf(typ string, list []entities.Entity) []entities.Change {
	out := make([]entities.Change, len(list))
	for i, g := range list {
		out[i] = entities.Change{Type: typ, Kind: g.Kind, ID: g.ID}
	}
	return out
}

// transition is the event for an entity whose state moved from before to after.
func transition(before, after entities.State) string {
	switch {
	case before != entities.StateDeleted && after == entities.StateDeleted:
		return entities.EventDeleted
	case before == entities.StateDeleted && after == entities.StateActive:
		return entities.EventRestored
	}
	return entities.EventUpdated
}
