package contentkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const obsoleteJobs = `kind = ANY($2::text[])
 AND jsonb_typeof(args->'tenant') = 'string' AND args->>'tenant' = ANY($1::text[])`

var offerJobKinds = []string{"contentkit_offer_sync", "contentkit_offer_reconcile"}
var publishingJobKinds = []string{"contentkit_publishing_operation", "contentkit_publishing_reconcile"}

func preflightOfferJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) error {
	return preflightRemovedJobs(ctx, pool, schema, tenants, offerJobKinds)
}
func retireOfferJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) error {
	return retireRemovedJobs(ctx, pool, schema, tenants, offerJobKinds)
}
func preflightPublishingJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) error {
	return preflightRemovedJobs(ctx, pool, schema, tenants, publishingJobKinds)
}
func retirePublishingJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) error {
	return retireRemovedJobs(ctx, pool, schema, tenants, publishingJobKinds)
}

// preflightRemovedJobs refuses old workers before their state is removed.
// It changes no jobs: pending work may still be needed if a schema migration
// refuses unfinished work. Operators must stop old writers first.
func preflightRemovedJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants, kinds []string) error {
	table, err := removedJobsTable(ctx, pool, schema, tenants)
	if err != nil || table == "" {
		return err
	}
	var id int64
	err = pool.QueryRow(ctx, `SELECT id FROM `+table+` WHERE `+obsoleteJobs+` AND state='running' ORDER BY id LIMIT 1`, tenants, kinds).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("contentkit: inspect obsolete jobs: %w", err)
	}
	return runningRemovedJob(id)
}

// retireRemovedJobs runs only after the schema migration succeeds. It
// cancels obsolete waiting work for these tenants, preserving all terminal
// history. Rechecking under row locks refuses a worker that started after the
// preflight; it is not a substitute for stopping old application instances.
func retireRemovedJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants, kinds []string) error {
	table, err := removedJobsTable(ctx, pool, schema, tenants)
	if err != nil || table == "" {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,state FROM `+table+` WHERE `+obsoleteJobs+`
 AND state IN ('available','pending','retryable','scheduled','running') ORDER BY id FOR UPDATE`, tenants, kinds)
		if err != nil {
			return fmt.Errorf("contentkit: lock obsolete jobs: %w", err)
		}
		type oldJob struct {
			id    int64
			state string
		}
		jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (oldJob, error) {
			var job oldJob
			err := row.Scan(&job.id, &job.state)
			return job, err
		})
		if err != nil {
			return fmt.Errorf("contentkit: read obsolete jobs: %w", err)
		}
		ids := make([]int64, 0, len(jobs))
		for _, job := range jobs {
			if job.state == "running" {
				return runningRemovedJob(job.id)
			}
			ids = append(ids, job.id)
		}
		if len(ids) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET state='cancelled',finalized_at=clock_timestamp()
 WHERE id=ANY($1::bigint[]) AND state IN ('available','pending','retryable','scheduled')`, ids)
		if err != nil {
			return fmt.Errorf("contentkit: cancel obsolete jobs: %w", err)
		}
		return nil
	})
}

func runningRemovedJob(id int64) error {
	return fmt.Errorf("contentkit: obsolete job %d is running; stop old ContentKit workers before migrating", id)
}

func removedJobsTable(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) (string, error) {
	if pool == nil {
		return "", errors.New("contentkit: job retirement requires PostgreSQL")
	}
	if len(tenants) == 0 {
		return "", nil
	}
	if schema == "" {
		schema = "public"
	}
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		return "", fmt.Errorf("contentkit: find obsolete job table: %w", err)
	}
	if !exists {
		return "", nil
	}
	return table, nil
}
