package workqueue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// JobStatus is one active queue, state, and priority in the worker's River schema.
// HighAttemptJobs counts attempts three and later. OldestAvailableSeconds is
// zero unless this group has a runnable available job.
type JobStatus struct {
	Queue                  string
	State                  string
	Priority               int
	Jobs                   int64
	HighAttemptJobs        int64
	OldestAvailableSeconds float64
}

// Snapshot reads the current worker backlog. Terminal jobs are excluded so
// retained job history cannot make an observability scrape increasingly costly.
func Snapshot(ctx context.Context, pool *pgxpool.Pool, schema string) ([]JobStatus, error) {
	if err := ValidSchema(schema); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
SELECT queue, state::text, priority, count(*), count(*) FILTER (WHERE attempt >= 3),
  coalesce(greatest(0, extract(epoch FROM now() - min(scheduled_at)
    FILTER (WHERE state = 'available' AND scheduled_at <= now()))), 0)::float8
FROM `+jobs(schema)+`
WHERE queue IN ('media_image', 'media_video_light', 'media_video_encode', 'media_audio')
  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
GROUP BY queue, state, priority`,
	)
	if err != nil {
		return nil, fmt.Errorf("media/workqueue: snapshot: %w", err)
	}
	defer rows.Close()

	var statuses []JobStatus
	for rows.Next() {
		var status JobStatus
		if err := rows.Scan(&status.Queue, &status.State, &status.Priority, &status.Jobs,
			&status.HighAttemptJobs, &status.OldestAvailableSeconds); err != nil {
			return nil, fmt.Errorf("media/workqueue: scan snapshot: %w", err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("media/workqueue: read snapshot: %w", err)
	}
	return statuses, nil
}

// TenantBacklog counts video jobs waiting to run, excluding running and
// terminal jobs. The tenant comes from the reference River actually stored.
func TenantBacklog(ctx context.Context, pool *pgxpool.Pool, schema string) (map[string]int64, error) {
	if err := ValidSchema(schema); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
SELECT args->'ref'->>'tenant_id', count(*)
FROM `+jobs(schema)+`
WHERE queue IN ('media_video_light', 'media_video_encode')
  AND state IN ('available', 'pending', 'retryable', 'scheduled')
  AND nullif(args->'ref'->>'tenant_id', '') IS NOT NULL
GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("media/workqueue: tenant backlog: %w", err)
	}
	defer rows.Close()
	backlog := make(map[string]int64)
	for rows.Next() {
		var tenant string
		var count int64
		if err := rows.Scan(&tenant, &count); err != nil {
			return nil, fmt.Errorf("media/workqueue: scan tenant backlog: %w", err)
		}
		backlog[tenant] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("media/workqueue: read tenant backlog: %w", err)
	}
	return backlog, nil
}
