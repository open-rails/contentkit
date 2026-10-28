package entitystore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	entities "github.com/open-rails/contentkit/entities"
)

// AddMembershipsTx adds associations in a caller-owned transaction, preserving
// every other shared and version-specific association. Authorize the mutation
// before opening tx; the same hierarchy-lock ordering as SetContentEntitiesTx applies.
func (s *Store) AddMembershipsTx(ctx context.Context, tx pgx.Tx, in []entities.Membership) error {
	if tx == nil {
		return errors.New("entities: transaction is required")
	}
	return s.bound(tx).AddMemberships(ctx, in)
}

// CreateEntitiesTx is CreateEntities in a caller-owned transaction. A store bound
// with As retains that actor and all its ordinary authorization checks. Channels require an explicit FinishChannels call after the caller commits;
// this method never starts background work.
func (s *Store) CreateEntitiesTx(ctx context.Context, tx pgx.Tx, in []entities.EntityInput) ([]entities.Entity, error) {
	if tx == nil {
		return nil, errors.New("entities: transaction is required")
	}
	return s.bound(tx).CreateEntities(ctx, in)
}

// SetContentEntitiesTx is SetContentEntities in a caller-owned transaction. Like
// SetContentEntities, its caller must first authorize editing those contents.
// It takes LockHierarchyShared; a tx that already holds entity or content row
// locks must take that first, or it can deadlock with a hierarchy change.
func (s *Store) SetContentEntitiesTx(ctx context.Context, tx pgx.Tx, in []entities.ContentEntities) error {
	if tx == nil {
		return errors.New("entities: transaction is required")
	}
	return s.bound(tx).SetContentEntities(ctx, in)
}

// PrepareStaffWrite resolves this As-bound store's staff permission before a
// caller holds a database transaction. The result is cached only on this
// actor-bound store, so subsequent transactional writes need no pool query.
// False is not an error: callers that only resolve existing taxonomy need not
// create any entities.
func (s *Store) PrepareStaffWrite(ctx context.Context) (bool, error) {
	if s.as == nil {
		return false, ErrForbidden
	}
	return s.staff(ctx)
}
