package signal

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// AnonymizeSubject moves an account's contributions to a fresh anonymous
// subject. The caller durably keeps that random key while this operation is
// pending, then discards the mapping when it succeeds. Repeating the same
// old/new pair is safe: the existing replacing-table keys deduplicate copies.
// The old account is permanently fenced before copying, so it cannot resume
// writing or appear in history while the operation is in progress.
func (st *Store) AnonymizeSubject(ctx context.Context, tenant string, old, anonymous Subject) error {
	if strings.TrimSpace(tenant) == "" || old.UserID == "" || old.AnonKey != "" || anonymous.AnonKey == "" || anonymous.UserID != "" {
		return fmt.Errorf("signal: anonymization requires a tenant, account and fresh anonymous subject")
	}
	if err := old.Validate(); err != nil {
		return err
	}
	if err := anonymous.Validate(); err != nil {
		return err
	}
	quorum, err := st.erasureQuorum(ctx)
	if err != nil {
		return err
	}
	tables, err := st.activeSubjectTables(ctx)
	if err != nil {
		return err
	}
	replicas, err := st.prepareAnonymousCopy(ctx, append(append([]string{}, tables...), "erasures"), quorum)
	if err != nil {
		return err
	}
	if err := st.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.erasures (tenant,subject_hash,erased_at)
SELECT ?,sipHash128(?),now64(6)%s`, st.db, quorumSetting(quorum)), tenant, subjectHashKey(old)); err != nil {
		return fmt.Errorf("signal: fence anonymized account: %w", err)
	}
	for pass := 0; pass < maxErasurePasses; pass++ {
		if err := st.syncAnonymousCopy(ctx, replicas); err != nil {
			return err
		}
		for _, table := range tables {
			if err := st.copyAnonymous(ctx, table, tenant, old, anonymous, quorum); err != nil {
				return err
			}
			if err := st.mutate(ctx, table, "tenant=? AND subject_kind=? AND subject=?", tenant, old.Kind(), old.Key()); err != nil {
				return err
			}
		}
		var remaining uint64
		for _, table := range tables {
			n, err := st.count(ctx, table, tenant, "subject_kind=? AND subject=?", []any{old.Kind(), old.Key()})
			if err != nil {
				return err
			}
			remaining += n
		}
		if remaining == 0 {
			return nil
		}
	}
	return fmt.Errorf("signal: account anonymization incomplete; retry")
}

// prepareAnonymousCopy verifies the complete replica set before any fence or
// mutation. Pooled connections must route only within this configured shard.
func (st *Store) prepareAnonymousCopy(ctx context.Context, tables []string, quorum uint32) ([]string, error) {
	if st.cluster == "" && quorum > 1 {
		return nil, fmt.Errorf("signal: anonymization requires a replica-affine copy source or a complete configured cluster; cleanup remains pending")
	}
	from := "system.replicas"
	var args []any
	if st.cluster != "" {
		from = "clusterAllReplicas(?,system.replicas)"
		args = append(args, st.cluster)
	}
	args = append(args, st.db, tables)
	rows, err := st.conn.Query(ctx, `SELECT table,zookeeper_name,zookeeper_path,replica_name,toUInt64(total_replicas),is_readonly,is_session_expired,arraySort(mapKeys(replica_is_active)) FROM `+from+` WHERE database=? AND table IN ? SETTINGS skip_unavailable_shards=0`, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: inspect anonymization replica set: %w", err)
	}
	type replicaSet struct {
		keeper, path string
		names        []string
		observed     map[string]bool
	}
	sets := map[string]*replicaSet{}
	for rows.Next() {
		var table, keeper, path, replica string
		var total uint64
		var readonly, expired uint8
		var names []string
		if err := rows.Scan(&table, &keeper, &path, &replica, &total, &readonly, &expired, &names); err != nil {
			rows.Close()
			return nil, err
		}
		if readonly != 0 || expired != 0 || len(names) != int(total) || total == 0 || (total > 1 && st.cluster == "") {
			rows.Close()
			return nil, fmt.Errorf("signal: incomplete or unhealthy anonymization replica set for %s", table)
		}
		set := sets[table]
		if set == nil {
			set = &replicaSet{keeper: keeper, path: path, names: names, observed: map[string]bool{}}
			sets[table] = set
		}
		if set.keeper != keeper || set.path != path || strings.Join(set.names, "\x00") != strings.Join(names, "\x00") {
			rows.Close()
			return nil, fmt.Errorf("signal: anonymization cluster spans inconsistent replica sets for %s", table)
		}
		set.observed[replica] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if st.cluster != "" {
		for _, table := range tables {
			set := sets[table]
			if set == nil || len(set.observed) != len(set.names) {
				return nil, fmt.Errorf("signal: anonymization cluster does not cover every replica of %s", table)
			}
			for _, replica := range set.names {
				if !set.observed[replica] {
					return nil, fmt.Errorf("signal: anonymization cluster is missing replica %s of %s", replica, table)
				}
			}
		}
	}
	var replicated []string
	for _, table := range tables {
		if sets[table] != nil {
			replicated = append(replicated, table)
		}
	}
	return replicated, nil
}

// Synchronize every source after the account fence commits. A local SYNC and
// a pooled copy could reach different replicas; ON CLUSTER covers both.
func (st *Store) syncAnonymousCopy(ctx context.Context, tables []string) error {
	// 'none' still waits and throws on host failure or timeout. A partially
	// completed SYNC is harmless; no copy/delete has begun when it returns.
	syncCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"distributed_ddl_output_mode": "none", "distributed_ddl_task_timeout": 60}))
	for _, table := range tables {
		onCluster := ""
		if st.cluster != "" {
			onCluster = " ON CLUSTER " + st.cluster
		}
		if err := st.conn.Exec(syncCtx, "SYSTEM SYNC REPLICA"+onCluster+" "+st.db+"."+table+" LIGHTWEIGHT"); err != nil {
			return fmt.Errorf("signal: synchronize %s before anonymization: %w", table, err)
		}
	}
	return nil
}

// copyAnonymous handles both the current schema and optional pre-retirement
// tables. Only the fixed subject-table allowlist reaches this method; columns
// come from ClickHouse's own schema. Free-form payloads and personal resume
// positions are not needed for anonymous aggregate statistics.
func (st *Store) copyAnonymous(ctx context.Context, table, tenant string, old, anonymous Subject, quorum uint32) error {
	rows, err := st.conn.Query(ctx, `SELECT name FROM system.columns WHERE database=? AND table=? AND default_kind NOT IN ('ALIAS','MATERIALIZED') ORDER BY position`, strings.Trim(st.db, "`"), table)
	if err != nil {
		return err
	}
	var columns, values []string
	var args []any
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return err
		}
		if !identRe.MatchString(column) {
			rows.Close()
			return fmt.Errorf("signal: invalid stored column %q", column)
		}
		columns = append(columns, "`"+column+"`")
		switch column {
		case "subject_kind":
			values = append(values, "?")
			args = append(args, anonymous.Kind())
		case "subject":
			values = append(values, "?")
			args = append(args, anonymous.Key())
		case "payload":
			values = append(values, "'{}'")
		case "resume":
			values = append(values, "''")
		case "version":
			if table == "exposures" {
				// Exposure identity excludes subject. The anonymous replacement
				// must beat the original even if a merge runs before deletion.
				values = append(values, "version+toUInt128(1)")
			} else {
				values = append(values, "`version`")
			}
		default:
			values = append(values, "`"+column+"`")
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("signal: missing subject table %s", table)
	}
	args = append(args, tenant, old.Kind(), old.Key())
	q := fmt.Sprintf(`INSERT INTO %s.%s (%s) SELECT %s FROM %s.%s FINAL
WHERE tenant=? AND subject_kind=? AND subject=?%s`, st.db, table, strings.Join(columns, ","), strings.Join(values, ","), st.db, table, quorumSetting(quorum))
	if err := st.conn.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("signal: anonymize %s: %w", table, err)
	}
	return nil
}
