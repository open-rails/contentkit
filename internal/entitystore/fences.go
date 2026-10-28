package entitystore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	entities "github.com/open-rails/contentkit/entities"
	"golang.org/x/sync/errgroup"
)

// FinishMedia makes the entities' media follow their committed state: the
// gateway serves none of a deleted entity's files until it is restored, and
// purge erases them for good. Like FinishChannels it runs after the write
// commits, reports failures and is safe to retry. Each entity's row stays
// share-locked through its storage calls, so a deletion, restore or purge
// claim commits before or after it, never during it.
func (s *Store) FinishMedia(ctx context.Context, ids []entities.EntityID) error {
	if s.tx != nil {
		return errors.New("entities: finish requires a committed transaction")
	}
	if s.folders == nil {
		return nil
	}
	list, err := uniqueIDs(ids)
	if err != nil || len(list) == 0 {
		return err
	}
	errs := make([]error, len(list))
	var g errgroup.Group
	g.SetLimit(8)
	for i, id := range list {
		g.Go(func() error {
			err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
				ref := entities.Ref{ID: entities.EntityID(id)}
				var deleted bool
				err := tx.QueryRow(ctx, "SELECT kind,state='deleted' FROM "+s.table("content_entities")+" WHERE tenant_id=$1 AND id=$2 FOR SHARE",
					s.tenant, id).Scan(&ref.Kind, &deleted)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				} else if err != nil {
					return err
				}
				return s.folders.FenceDeleted(ctx, ref, deleted)
			})
			if err != nil {
				errs[i] = fmt.Errorf("entities: fence %s media: %w", id, err)
			}
			return nil
		})
	}
	_ = g.Wait()
	return errors.Join(errs...)
}
