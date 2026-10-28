package signalstore

import (
	"context"
	"fmt"
	"strings"

	signal "github.com/open-rails/contentkit/signal"
)

// Inventory reports canonical and raw event volume per content kind and signal type.
func (st *Store) Inventory(ctx context.Context, tenant string) ([]signal.InventoryRow, error) {
	if strings.TrimSpace(tenant) == "" {
		return nil, fmt.Errorf("signal: tenant is required")
	}
	q := fmt.Sprintf(`SELECT content_kind, signal_type, count(), sum(raw), uniqExact(subject_kind, subject), uniqExact(content_id), min(at), max(at)
FROM (
    SELECT %[3]s, subject_kind, subject, signal_type, event_id, argMax(occurred_at, version) AS at, count() AS raw
    FROM %[1]s.signals
    WHERE tenant = ? AND signal_type NOT IN ('view','preview') AND %[2]s
    GROUP BY %[3]s, subject_kind, subject, signal_type, event_id
 UNION ALL
 SELECT %[3]s,subject_kind,subject,'engagement' AS signal_type,receipt_id AS event_id,argMax(occurred_at,version) AS at,count() AS raw
 FROM %[1]s.engagement_segments WHERE tenant=? AND %[2]s
 GROUP BY %[3]s,subject_kind,subject,receipt_id
)
GROUP BY content_kind, signal_type
ORDER BY content_kind, signal_type`, st.db, st.notErased(), refColumns)
	rows, err := st.conn.Query(ctx, q, tenant, tenant)
	if err != nil {
		return nil, fmt.Errorf("signal: inventory: %w", err)
	}
	defer rows.Close()
	var out []signal.InventoryRow
	for rows.Next() {
		var r signal.InventoryRow
		if err := rows.Scan(&r.ContentKind, &r.SignalType, &r.Events, &r.RawRows, &r.Subjects, &r.ContentItems, &r.FirstAt, &r.LastAt); err != nil {
			return nil, fmt.Errorf("signal: inventory scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PurgeContentKinds deletes a tenant's events and projections for whole content
// kinds (for example retired fan-out rows) and waits
// for the mutations on every replica. Irreversible; inventory first.
func (st *Store) PurgeContentKinds(ctx context.Context, tenant string, contentKinds []string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	kinds := trimAll(contentKinds)
	if len(kinds) == 0 {
		return fmt.Errorf("signal: contentKinds are required")
	}
	for _, table := range subjectTables {
		if table == "exposures" {
			continue
		}
		if err := st.mutate(ctx, table, "tenant = ? AND content_kind IN ?", tenant, kinds); err != nil {
			return err
		}
	}
	for _, table := range []string{"popularity_daily", "popularity_daily_staging", "popularity_daily_prepared"} {
		if err := st.mutate(ctx, table, "tenant = ? AND content_kind IN ?", tenant, kinds); err != nil {
			return err
		}
	}
	return nil
}

// mutate runs ALTER TABLE DELETE and waits for completion on all replicas.
func (st *Store) mutate(ctx context.Context, table, where string, args ...any) error {
	q := fmt.Sprintf("ALTER TABLE %s.%s DELETE WHERE %s SETTINGS mutations_sync = 2", st.db, table, where)
	if err := st.conn.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("signal: delete from %s: %w", table, err)
	}
	return nil
}
