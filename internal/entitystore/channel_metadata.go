package entitystore

import (
	"context"
	"fmt"
	"slices"

	entities "github.com/open-rails/contentkit/entities"
)

// ChannelMetadata returns descriptive metadata on a reserved channel. This
// association never enters content facets or grants channel/post authority.
func (s *Store) ChannelMetadata(ctx context.Context, id entities.EntityID) ([]entities.EntityID, error) {
	if !s.channels {
		return nil, ErrForbidden
	}
	if err := validateID(id); err != nil {
		return nil, err
	}
	out := []entities.EntityID{}
	err := s.read(ctx, func(q Querier) error {
		found, err := s.byIDs(ctx, q, []string{string(id)})
		if err != nil {
			return err
		}
		if len(found) != 1 {
			return ErrNotFound
		}
		rows, err := q.Query(ctx, `SELECT entity_id FROM `+s.table("content_channel_entities")+` WHERE tenant_id=$1 AND channel_id=$2 ORDER BY entity_id`, s.tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var eid entities.EntityID
			if err := rows.Scan(&eid); err != nil {
				return err
			}
			out = append(out, eid)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) SetChannelMetadata(ctx context.Context, id entities.EntityID, ids []entities.EntityID) error {
	if !s.channels || s.cfg.Channels == nil {
		return ErrForbidden
	}
	if err := validateID(id); err != nil {
		return err
	}
	wanted, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	before, err := s.Entities(ctx, []entities.EntityID{id})
	if err != nil {
		return err
	}
	if len(before) != 1 {
		return ErrNotFound
	}
	if err := s.mayEdit(ctx, before); err != nil {
		return err
	}
	return s.run(ctx, func(q Querier) error {
		if err := s.LockHierarchyShared(ctx, q); err != nil {
			return err
		}
		locked, err := s.lock(ctx, q, []string{string(id)})
		if err != nil {
			return err
		}
		if locked[0].State != entities.StateActive {
			return ErrNotFound
		}
		if locked[0].Revision != before[0].Revision || locked[0].AuthScope != before[0].AuthScope {
			return ErrConflict
		}
		metadata := *s
		metadata.channels = false
		found, err := metadata.byIDs(ctx, q, wanted)
		if err != nil {
			return err
		}
		if len(found) != len(wanted) {
			return ErrNotFound
		}
		for _, e := range found {
			if e.State != entities.StateActive || e.Internal || !slices.Contains(s.cfg.Channels.Entities, e.Kind) {
				return fmt.Errorf("%w: metadata kind %s is not enabled for channels", entities.ErrInvalid, e.Kind)
			}
		}
		if _, err := q.Exec(ctx, `DELETE FROM `+s.table("content_channel_entities")+` WHERE tenant_id=$1 AND channel_id=$2`, s.tenant, id); err != nil {
			return err
		}
		if len(wanted) > 0 {
			if _, err := q.Exec(ctx, `INSERT INTO `+s.table("content_channel_entities")+`(tenant_id,channel_id,entity_id) SELECT $1,$2,unnest($3::text[])`, s.tenant, id, wanted); err != nil {
				return err
			}
		}
		return s.touch(ctx, q, locked)
	})
}
