package taxonomy

import (
	"context"
	"fmt"
	"time"
)

// SetImportedTimestamps preserves source chronology during a trusted import.
// Nil leaves a timestamp unchanged; at least one nonzero timestamp is required.
// This operation is deliberately separate from the public HTTP mutation inputs.
// Call it after the imported node's other mutations, which normally set
// updated_at to the current transaction time. It neither changes source_revision
// nor commits a transaction borrowed through WithTx or WithSQLTx.
func (s *Store) SetImportedTimestamps(ctx context.Context, id TaxonomyID, createdAt, updatedAt *time.Time) error {
	if err := validateID(id); err != nil {
		return err
	}
	if createdAt == nil && updatedAt == nil {
		return fmt.Errorf("%w: an imported timestamp is required", ErrInvalid)
	}
	if createdAt != nil && createdAt.IsZero() || updatedAt != nil && updatedAt.IsZero() {
		return fmt.Errorf("%w: imported timestamps must be nonzero", ErrInvalid)
	}
	if createdAt != nil {
		value := createdAt.UTC()
		createdAt = &value
	}
	if updatedAt != nil {
		value := updatedAt.UTC()
		updatedAt = &value
	}
	return s.run(ctx, func(q querier) error {
		node, err := s.lockNode(ctx, q, id)
		if err != nil {
			return err
		}
		if node.State == StateMerged {
			return fmt.Errorf("%w: node %s is merged", ErrConflict, id)
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET created_at=COALESCE($3::timestamptz,created_at),updated_at=COALESCE($4::timestamptz,updated_at) WHERE tenant_id=$1 AND taxonomy_id=$2`, s.table("content_nodes")), s.tenant, string(id), createdAt, updatedAt); err != nil {
			return err
		}
		return s.markDirty(ctx, q, []nodeKey{{node.Kind, node.TaxonomyID}}, node.State != StateActive)
	})
}
