package contentkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const obsoleteOfferJobs = `kind IN ('contentkit_offer_sync','contentkit_offer_reconcile')
 AND jsonb_typeof(args->'tenant') = 'string' AND args->>'tenant' = ANY($1::text[])`

// preflightOfferJobs refuses a running old writer before the offer table is
// removed. It changes no jobs: pending work may still be needed if the schema
// migration refuses unsettled offers. Operators must stop old writers first.
func preflightOfferJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) error {
	table, err := offerJobsTable(ctx, pool, schema, tenants)
	if err != nil || table == "" {
		return err
	}
	var id int64
	err = pool.QueryRow(ctx, `SELECT id FROM `+table+` WHERE `+obsoleteOfferJobs+` AND state='running' ORDER BY id LIMIT 1`, tenants).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("contentkit: inspect obsolete offer jobs: %w", err)
	}
	return runningOfferJob(id)
}

// retireOfferJobs runs only after the offer schema migration succeeds. It
// cancels obsolete waiting work for these tenants, preserving all terminal
// history. Rechecking under row locks refuses a worker that started after the
// preflight; it is not a substitute for stopping old application instances.
func retireOfferJobs(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) error {
	table, err := offerJobsTable(ctx, pool, schema, tenants)
	if err != nil || table == "" {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,state FROM `+table+` WHERE `+obsoleteOfferJobs+`
 AND state IN ('available','pending','retryable','scheduled','running') ORDER BY id FOR UPDATE`, tenants)
		if err != nil {
			return fmt.Errorf("contentkit: lock obsolete offer jobs: %w", err)
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
			return fmt.Errorf("contentkit: read obsolete offer jobs: %w", err)
		}
		ids := make([]int64, 0, len(jobs))
		for _, job := range jobs {
			if job.state == "running" {
				return runningOfferJob(job.id)
			}
			ids = append(ids, job.id)
		}
		if len(ids) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET state='cancelled',finalized_at=clock_timestamp()
 WHERE id=ANY($1::bigint[]) AND state IN ('available','pending','retryable','scheduled')`, ids)
		if err != nil {
			return fmt.Errorf("contentkit: cancel obsolete offer jobs: %w", err)
		}
		return nil
	})
}

func runningOfferJob(id int64) error {
	return fmt.Errorf("contentkit: obsolete offer job %d is running; stop old ContentKit workers before migrating", id)
}

func offerJobsTable(ctx context.Context, pool *pgxpool.Pool, schema string, tenants []string) (string, error) {
	if pool == nil {
		return "", errors.New("contentkit: offer job retirement requires PostgreSQL")
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
		return "", fmt.Errorf("contentkit: find obsolete offer job table: %w", err)
	}
	if !exists {
		return "", nil
	}
	return table, nil
}
