package signalstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Erase records subjects in erasures, on every replica (a quorum insert):
// from then on every read of activity drops their rows, and DeleteErased
// removes them. Idempotent.
func (st *Store) Erase(ctx context.Context, tenant string, subjects []uuid.UUID, at time.Time) error {
	if len(subjects) == 0 {
		return nil
	}
	quorum, err := st.quorum(ctx)
	if err != nil {
		return err
	}
	values := make([]string, len(subjects))
	args := make([]any, 0, 3*len(subjects))
	for i, s := range subjects {
		values[i] = "(?, ?, ?)"
		args = append(args, tenant, s.String(), at.UTC().Format(tsLayout))
	}
	q := "INSERT INTO " + st.db + ".erasures (tenant, subject, erased_at)"
	if quorum > 1 {
		q += fmt.Sprintf(" SETTINGS insert_quorum = %d, insert_quorum_parallel = 1", quorum)
	}
	if err := st.conn.Exec(ctx, q+" VALUES "+strings.Join(values, ", "), args...); err != nil {
		return fmt.Errorf("signal: record erasures: %w", err)
	}
	return nil
}

// quorum is the replica count Keeper expects for erasures, so a down
// replica fails an erasure rather than leaving it unrecorded there.
func (st *Store) quorum(ctx context.Context) (uint32, error) {
	rows, err := st.conn.Query(ctx, "SELECT max(toUInt32(total_replicas)) FROM system.replicas WHERE database = ? AND table = 'erasures'", st.db)
	if err != nil {
		return 0, fmt.Errorf("signal: erasure quorum: %w", err)
	}
	defer rows.Close()
	var n uint32
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// eraseBatch bounds one lightweight DELETE's literal subject list.
const eraseBatch = 500

// DeleteErased deletes the activity rows of the subjects erased at or after
// since: the daily lightweight DELETE behind the read-time filter. A
// replicated delete must be deterministic, so it names the subjects rather
// than reading erasures. It returns the newest erasure handled, zero when
// there is none.
func (st *Store) DeleteErased(ctx context.Context, tenant string, since time.Time) (time.Time, error) {
	where, args := "tenant = ?", []any{tenant}
	if !since.IsZero() {
		where, args = where+" AND erased_at >= ?", append(args, since.UTC().Format(tsLayout))
	}
	rows, err := st.conn.Query(ctx, `SELECT subject, max(erased_at) FROM `+st.db+`.erasures WHERE `+where+` GROUP BY subject`, args...)
	if err != nil {
		return time.Time{}, fmt.Errorf("signal: erased subjects: %w", err)
	}
	var subjects []uuid.UUID
	var newest time.Time
	for rows.Next() {
		var id uuid.UUID
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return time.Time{}, err
		}
		subjects = append(subjects, id)
		if at.After(newest) {
			newest = at
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	for start := 0; start < len(subjects); start += eraseBatch {
		chunk := uuidStrings(subjects[start:min(start+eraseBatch, len(subjects))])
		if err := st.conn.Exec(ctx, `DELETE FROM `+st.db+`.activity WHERE tenant = ? AND subject IN ?`, tenant, chunk); err != nil {
			return time.Time{}, fmt.Errorf("signal: delete erased activity: %w", err)
		}
	}
	return newest, nil
}

// Forget deletes subjects' activity: of the content in ids, of one kind
// (ids empty) or all of it (kind and ids empty).
func (st *Store) Forget(ctx context.Context, tenant string, subjects []uuid.UUID, kind string, ids []uuid.UUID) error {
	if len(subjects) == 0 {
		return nil
	}
	where, args := "tenant = ? AND subject IN ?", []any{tenant, uuidStrings(subjects)}
	switch {
	case len(ids) > 0:
		where, args = where+" AND content_id IN ?", append(args, uuidStrings(ids))
	case kind != "":
		where, args = where+" AND content_kind = ?", append(args, kind)
	}
	if err := st.conn.Exec(ctx, `DELETE FROM `+st.db+`.activity WHERE `+where, args...); err != nil {
		return fmt.Errorf("signal: forget activity: %w", err)
	}
	return nil
}

// ForgetContents deletes purged content from the log and its rollups.
func (st *Store) ForgetContents(ctx context.Context, tenant string, ids []uuid.UUID) error {
	for start := 0; start < len(ids); start += 1000 {
		chunk := uuidStrings(ids[start:min(start+1000, len(ids))])
		for _, table := range []string{"activity", "content_daily"} {
			if err := st.conn.Exec(ctx, `DELETE FROM `+st.db+`.`+table+` WHERE tenant = ? AND content_id IN ?`, tenant, chunk); err != nil {
				return fmt.Errorf("signal: forget content in %s: %w", table, err)
			}
		}
	}
	return nil
}
