package groups

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

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
