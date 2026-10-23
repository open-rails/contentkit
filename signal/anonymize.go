package signal

import (
	"context"
	"fmt"
	"strings"
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
	if err := st.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.erasures (tenant,subject_hash,erased_at)
SELECT ?,sipHash128(?),now64(6)%s`, st.db, quorumSetting(quorum)), tenant, subjectHashKey(old)); err != nil {
		return fmt.Errorf("signal: fence anonymized account: %w", err)
	}
	tables, err := st.activeSubjectTables(ctx)
	if err != nil {
		return err
	}
	for pass := 0; pass < maxErasurePasses; pass++ {
		for _, table := range tables {
			// Pull accepted parts from other replicas before deleting the old
			// account everywhere. FINAL alone does not synchronize replicas.
			rows, err := st.conn.Query(ctx, "SELECT count() FROM system.replicas WHERE database=? AND table=?", st.db, table)
			if err != nil {
				return err
			}
			var replicated uint64
			if rows.Next() {
				err = rows.Scan(&replicated)
			}
			rows.Close()
			if err != nil {
				return err
			}
			if replicated != 0 {
				if err := st.conn.Exec(ctx, "SYSTEM SYNC REPLICA "+st.db+"."+table+" LIGHTWEIGHT"); err != nil {
					return fmt.Errorf("signal: synchronize %s before anonymization: %w", table, err)
				}
			}
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
