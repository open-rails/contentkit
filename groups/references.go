package groups

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// References maintains a host's links to catalog groups. Calls run with the
// affected groups locked, in the same transaction as the catalog mutation.
// Implementations must use q, not another connection, for all database work;
// an error aborts the mutation. Tenant is the store's catalog key.
// Reference-aware mutations require READ COMMITTED transactions so checks see
// host links committed while waiting for a group's lock. Callers of Tx methods
// must roll back their transaction on error.
type References interface {
	// CheckUnavailable rejects deletion or public-to-internal reclassification
	// while host links still require the groups. Return ErrConflict to refuse.
	CheckUnavailable(ctx context.Context, q Querier, tenant string, refs []Ref) error
	// Merge moves From's host links to Into, collapsing duplicate pairs.
	Merge(ctx context.Context, q Querier, tenant string, from, into Ref) error
}

// LockReferencesTx validates active, non-internal groups of the exact requested
// kinds and locks them until tx ends. A host must call this before writing its
// own links, in that same transaction, to serialize with merges and deletion.
// Pass the complete selection together so locks are acquired in ID order.
// This is a storage contract, not authorization: callers authorize the host
// operation before starting tx. It does not follow merged IDs or create groups.
// tx must use READ COMMITTED isolation; roll it back on error.
func (s *Store) LockReferencesTx(ctx context.Context, tx pgx.Tx, refs []Ref) error {
	if tx == nil {
		return fmt.Errorf("%w: transaction is required", ErrInvalid)
	}
	if len(refs) == 0 {
		return nil
	}
	if err := checkReferenceTransaction(ctx, tx); err != nil {
		return err
	}
	ids := make([]GroupID, len(refs))
	wanted := make(map[GroupID]string, len(refs))
	for i, ref := range refs {
		if _, err := s.groupKind(ref.Kind); err != nil {
			return err
		}
		if previous, ok := wanted[ref.ID]; ok && previous != ref.Kind {
			return fmt.Errorf("%w: group %s has conflicting kinds", ErrInvalid, ref.ID)
		}
		wanted[ref.ID] = ref.Kind
		ids[i] = ref.ID
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g
 WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) ORDER BY g.id FOR SHARE`, groupColumns, s.table("content_groups")), s.tenant, list)
	if err != nil {
		return err
	}
	locked, err := scanGroups(rows)
	if err != nil {
		return err
	}
	if len(locked) != len(list) {
		return ErrNotFound
	}
	for _, g := range locked {
		if g.Kind != wanted[g.ID] {
			return fmt.Errorf("%w: group %s is not a %s", ErrInvalid, g.ID, wanted[g.ID])
		}
		if g.State != StateActive || g.Internal {
			return fmt.Errorf("%w: group %s is unavailable", ErrConflict, g.ID)
		}
	}
	return nil
}

func checkReferenceTransaction(ctx context.Context, q Querier) error {
	var isolation string
	if err := q.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		return err
	}
	if isolation != "read committed" {
		return fmt.Errorf("%w: host group references require READ COMMITTED isolation", ErrInvalid)
	}
	return nil
}
