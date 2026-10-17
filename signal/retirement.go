package signal

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

var retiredTables = []string{"subject_content_state", "subject_content_daily", "content_pairs", "population_observations", "population_coverage"}
var retiredSourceColumns = map[string]string{"duration_s": "UInt32", "progress": "UInt32", "progress_max": "UInt32", "score": "Int16", "completed": "Bool", "resume": "String"}

// RetirementOptions makes the post-deployment barrier explicit. This operation
// is never called by app startup or ordinary migrations. Every app, worker and
// erasure executor sharing the database must already run the new writer cohort.
type RetirementOptions struct {
	LegacyWritersStopped bool
	Cluster              string
}

// RetireOpenCounters refreshes legacy resume/progress one last time and removes
// obsolete raw-open data/projections. It is idempotent, but requires a verified
// backup and an operator-confirmed stopped legacy writer cohort.
func (st *Store) RetireOpenCounters(ctx context.Context, o RetirementOptions) error {
	if !o.LegacyWritersStopped {
		return fmt.Errorf("signal: legacy counter retirement requires all legacy writers to be stopped")
	}
	if o.Cluster != "" && !identRe.MatchString(o.Cluster) {
		return fmt.Errorf("signal: invalid cluster name")
	}
	cluster := ""
	if o.Cluster != "" {
		cluster = " ON CLUSTER " + o.Cluster
	}
	tables, err := st.existing(ctx, retiredTables)
	if err != nil {
		return err
	}
	if slices.Contains(tables, "subject_content_state") {
		q := fmt.Sprintf(`INSERT INTO %s.legacy_reading_state SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,
 first_seen_at,last_view_at,max_progress,progress_max,completed,resume,
 bitOr(bitShiftLeft(toUInt256(toUnixTimestamp64Micro(toDateTime64(last_view_at,6,'UTC'))),128),toUInt256(cityHash64(first_seen_at,last_view_at,max_progress,progress_max,completed,resume))) FROM %s.subject_content_state FINAL
 WHERE (max_progress>0 OR completed OR resume!='') AND %s`, st.db, st.db, st.notErased())
		if err := st.conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("signal: final legacy resume copy: %w", err)
		}
	}
	// Synchronous replicated mutations finish before source columns/tables vanish.
	if err := st.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.signals DELETE WHERE signal_type IN ('view','preview') SETTINGS mutations_sync=2", st.db)); err != nil {
		return err
	}
	q := fmt.Sprintf("ALTER TABLE %s.signals%s MODIFY COLUMN version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision),64),toUInt128(cityHash64(occurred_at,value,payload)))", st.db, cluster)
	if err := st.conn.Exec(ctx, q); err != nil {
		return err
	}
	var drops []string
	for _, name := range sortedKeys(retiredSourceColumns) {
		drops = append(drops, "DROP COLUMN IF EXISTS "+name)
	}
	if err := st.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.signals%s %s", st.db, cluster, strings.Join(drops, ", "))); err != nil {
		return err
	}
	for _, table := range tables {
		if err := st.conn.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s.%s%s SYNC", st.db, table, cluster)); err != nil {
			return err
		}
	}
	rows, err := st.conn.Query(ctx, "SELECT DISTINCT tenant FROM "+st.db+".erasures")
	if err != nil {
		return err
	}
	var tenants []string
	for rows.Next() {
		var tenant string
		if err := rows.Scan(&tenant); err != nil {
			rows.Close()
			return err
		}
		tenants = append(tenants, tenant)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if _, err := st.EnforceErasures(ctx, tenant); err != nil {
			return err
		}
	}
	return nil
}

func (st *Store) existing(ctx context.Context, names []string) ([]string, error) {
	rows, err := st.conn.Query(ctx, "SELECT name FROM system.tables WHERE database=? AND name IN ?", st.db, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Erasure removes old cohort residue while it exists, and keeps working after
// the explicit retirement has removed those optional tables.
func (st *Store) activeSubjectTables(ctx context.Context) ([]string, error) {
	optional, err := st.existing(ctx, retiredTables)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, table := range subjectTables {
		if !slices.Contains(retiredTables, table) || slices.Contains(optional, table) {
			out = append(out, table)
		}
	}
	return out, nil
}
