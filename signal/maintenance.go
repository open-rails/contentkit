package signal

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// InventoryRow sizes one entity type × signal type of a tenant's canonical
// events: the evidence for deciding which signals to keep collecting.
type InventoryRow struct {
	EntityType string
	SignalType string
	Events     uint64 // canonical events
	RawRows    uint64 // stored rows incl. unmerged superseded revisions and retries
	Subjects   uint64
	Entities   uint64
	FirstAt    time.Time
	LastAt     time.Time
}

// Inventory reports canonical and raw event volume per entity and signal type.
func (st *Store) Inventory(ctx context.Context, tenant string) ([]InventoryRow, error) {
	if strings.TrimSpace(tenant) == "" {
		return nil, fmt.Errorf("signal: tenant is required")
	}
	q := fmt.Sprintf(`SELECT entity_type, signal_type, count(), sum(raw), uniqExact(subject_kind, subject), uniqExact(entity_id), min(at), max(at)
FROM (
    SELECT entity_type, entity_id, subject_kind, subject, signal_type, event_id, argMax(occurred_at, version) AS at, count() AS raw
    FROM %s.events
    WHERE tenant = ?
    GROUP BY entity_type, entity_id, subject_kind, subject, signal_type, event_id
)
GROUP BY entity_type, signal_type
ORDER BY entity_type, signal_type`, st.db)
	rows, err := st.conn.Query(ctx, q, tenant)
	if err != nil {
		return nil, fmt.Errorf("signal: inventory: %w", err)
	}
	defer rows.Close()
	var out []InventoryRow
	for rows.Next() {
		var r InventoryRow
		if err := rows.Scan(&r.EntityType, &r.SignalType, &r.Events, &r.RawRows, &r.Subjects, &r.Entities, &r.FirstAt, &r.LastAt); err != nil {
			return nil, fmt.Errorf("signal: inventory scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PurgeEntityTypes deletes a tenant's events, projections and co-engagement
// pairs for whole entity types (for example retired per-parent fan-out rows)
// and waits for the mutations on every replica. Irreversible; inventory first.
func (st *Store) PurgeEntityTypes(ctx context.Context, tenant string, entityTypes []string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	types := trimAll(entityTypes)
	if len(types) == 0 {
		return fmt.Errorf("signal: entityTypes are required")
	}
	for _, table := range []string{"events", "subject_state", "subject_daily"} {
		if err := st.mutate(ctx, table, "tenant = ? AND entity_type IN ?", tenant, types); err != nil {
			return err
		}
	}
	return st.mutate(ctx, "item_pairs", "tenant = ? AND (entity_type_a IN ? OR entity_type_b IN ?)", tenant, types, types)
}

// mutate runs ALTER TABLE DELETE and waits for completion on all replicas.
func (st *Store) mutate(ctx context.Context, table, where string, args ...any) error {
	q := fmt.Sprintf("ALTER TABLE %s.%s DELETE WHERE %s SETTINGS mutations_sync = 2", st.db, table, where)
	if err := st.conn.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("signal: delete from %s: %w", table, err)
	}
	return nil
}
