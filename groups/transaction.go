package groups

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// AddMembershipsTx adds associations in a caller-owned transaction, preserving
// every other shared and version-specific association. Authorize the mutation
// before opening tx; the same hierarchy-lock ordering as SetContentGroupsTx applies.
func (s *Store) AddMembershipsTx(ctx context.Context, tx pgx.Tx, in []Membership) error {
	if tx == nil {
		return errors.New("groups: transaction is required")
	}
	return s.bound(tx).AddMemberships(ctx, in)
}

// CreateGroupsTx is CreateGroups in a caller-owned transaction. A store bound
// with As retains that actor and all its ordinary authorization checks. Scope
// provisioning is left to the existing transactional sync job after commit.
func (s *Store) CreateGroupsTx(ctx context.Context, tx pgx.Tx, in []GroupInput) ([]Group, error) {
	if tx == nil {
		return nil, errors.New("groups: transaction is required")
	}
	return s.bound(tx).CreateGroups(ctx, in)
}

// SetContentGroupsTx is SetContentGroups in a caller-owned transaction. Like
// SetContentGroups, its caller must first authorize editing those contents.
// It takes LockHierarchyShared; a tx that already holds group or content row
// locks must take that first, or it can deadlock with a hierarchy change.
func (s *Store) SetContentGroupsTx(ctx context.Context, tx pgx.Tx, in []ContentGroups) error {
	if tx == nil {
		return errors.New("groups: transaction is required")
	}
	return s.bound(tx).SetContentGroups(ctx, in)
}

// PrepareStaffWrite resolves this As-bound store's staff permission before a
// caller holds a database transaction. The result is cached only on this
// actor-bound store, so subsequent transactional writes need no pool query.
// False is not an error: callers that only resolve existing taxonomy need not
// create any groups.
func (s *Store) PrepareStaffWrite(ctx context.Context) (bool, error) {
	if s.as == nil {
		return false, ErrForbidden
	}
	return s.staff(ctx)
}
