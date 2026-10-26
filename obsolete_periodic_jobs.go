package contentkit

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Retired periodic jobs used empty args and a shared queue. Neither identifies
// an app, so only an operator's exact reviewed IDs authorize retirement.
var obsoletePeriodicJobKinds = []string{"contentkit_reclassify"}

func (e *Engine) obsoletePeriodicJobsTable(ctx context.Context) (string, error) {
	return removedJobsTable(ctx, e.pool, e.riverSchema, []string{e.cfg.Tenant})
}

func inspectObsoletePeriodicJobs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, table string, selected []int64, lock bool) ([]int64, error) {
	ids := slices.Compact(slices.Sorted(slices.Values(selected)))
	if len(ids) > 0 && ids[0] <= 0 {
		return nil, errors.New("contentkit: obsolete periodic job retirement IDs must be positive")
	}
	query := `SELECT id,kind,queue,state,args='{}'::jsonb FROM ` + table + `
 WHERE id=ANY($1::bigint[]) OR (kind=ANY($3::text[]) AND queue=$2
 AND state IN ('available','pending','retryable','scheduled','running')) ORDER BY id`
	if lock {
		query += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, query, ids, QueueContentKit, obsoletePeriodicJobKinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found, waiting []int64
	for rows.Next() {
		var id int64
		var kind, queue, state string
		var empty bool
		if err := rows.Scan(&id, &kind, &queue, &state, &empty); err != nil {
			return nil, err
		}
		if !slices.Contains(ids, id) {
			return nil, fmt.Errorf("contentkit: obsolete periodic job %d has no app identity; stop the owning fleet, review its exact IDs, then use MigrationOptions.RetireObsoleteJobs", id)
		}
		if !slices.Contains(obsoletePeriodicJobKinds, kind) || queue != QueueContentKit || !empty {
			return nil, fmt.Errorf("contentkit: job %d does not match a retired empty-argument periodic job; leave foreign or ambiguous work untouched", id)
		}
		found = append(found, id)
		switch state {
		case "running":
			return nil, runningRemovedJob(id)
		case "available", "pending", "retryable", "scheduled":
			waiting = append(waiting, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !slices.Equal(ids, found) {
		return nil, errors.New("contentkit: a selected obsolete periodic job does not exist in this River schema")
	}
	return waiting, nil
}

func (e *Engine) preflightObsoletePeriodicJobs(ctx context.Context, ids []int64) error {
	table, err := e.obsoletePeriodicJobsTable(ctx)
	if err != nil || table == "" {
		if err == nil && len(ids) != 0 {
			return errors.New("contentkit: selected obsolete periodic jobs require an existing River schema")
		}
		return err
	}
	_, err = inspectObsoletePeriodicJobs(ctx, e.pool, table, ids, false)
	return err
}

func (e *Engine) retireObsoletePeriodicJobs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return e.preflightObsoletePeriodicJobs(ctx, nil)
	}
	table, err := e.obsoletePeriodicJobsTable(ctx)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		waiting, err := inspectObsoletePeriodicJobs(ctx, tx, table, ids, true)
		if err != nil || len(waiting) == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET state='cancelled',finalized_at=clock_timestamp(),unique_key=NULL
 WHERE id=ANY($1::bigint[]) AND state IN ('available','pending','retryable','scheduled')`, waiting)
		return err
	})
}
