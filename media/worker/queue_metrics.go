package worker

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media/workqueue"
)

var (
	jobsDesc = prometheus.NewDesc("media_video_jobs", "Active media jobs in the worker's River schema.",
		[]string{"queue", "state", "priority"}, nil)
	highAttemptDesc = prometheus.NewDesc("media_video_high_attempt_jobs", "Active media jobs on attempt three or later.",
		[]string{"queue", "state", "priority"}, nil)
	oldestAvailableDesc = prometheus.NewDesc("media_video_oldest_available_seconds", "Age of the oldest runnable media job, or zero when none is runnable.",
		[]string{"queue", "priority"}, nil)
	tenantBacklogDesc = prometheus.NewDesc("media_video_tenant_backlog", "Derivative jobs waiting to run by tenant.",
		[]string{"tenant"}, nil)
	collectionSuccessDesc = prometheus.NewDesc("media_video_queue_collection_success", "Whether the current queue snapshot was read successfully (1) or failed (0).", nil, nil)
)

var (
	queues       = [...]string{workqueue.IngestQueue, workqueue.DerivativeQueue}
	activeStates = [...]string{"available", "pending", "retryable", "running", "scheduled"}
)

// queueCollector exports the worker schema's backlog. Only the worker holding
// River leadership of that schema polls and exports it, so several workers
// never duplicate the gauges.
type queueCollector struct {
	mu        sync.RWMutex
	pool      *pgxpool.Pool
	jobs      string
	term      uint64
	active    bool
	collected bool
	success   bool
	statuses  []jobStatus
	backlog   map[string]int64
}

type jobStatus struct {
	queue, state           string
	priority               int
	jobs, highAttempts     int64
	oldestAvailableSeconds float64
}

// watch binds the collector to the first worker built with it and returns
// that worker's leadership hook; later workers get nil.
func (c *queueCollector) watch(pool *pgxpool.Pool, schema string) rivertype.Hook {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool != nil {
		return nil
	}
	c.pool, c.jobs = pool, pgx.Identifier{schema, "river_job"}.Sanitize()
	return river.HookPeriodicJobsStartFunc(func(ctx context.Context, _ *rivertype.HookPeriodicJobsStartParams) error {
		c.start(ctx)
		return nil
	})
}

func (*queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- jobsDesc
	ch <- highAttemptDesc
	ch <- oldestAvailableDesc
	ch <- tenantBacklogDesc
	ch <- collectionSuccessDesc
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.active || !c.collected {
		return
	}
	if !c.success {
		ch <- prometheus.MustNewConstMetric(collectionSuccessDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(collectionSuccessDesc, prometheus.GaugeValue, 1)
	for tenant, jobs := range c.backlog {
		ch <- prometheus.MustNewConstMetric(tenantBacklogDesc, prometheus.GaugeValue, float64(jobs), tenant)
	}
	type group struct {
		queue, state string
		priority     int
	}
	byGroup := make(map[group]jobStatus, len(c.statuses))
	for _, s := range c.statuses {
		byGroup[group{s.queue, s.state, s.priority}] = s
	}
	for _, queue := range queues {
		for priority := 1; priority <= 4; priority++ {
			label := strconv.Itoa(priority)
			for _, state := range activeStates {
				s := byGroup[group{queue, state, priority}]
				ch <- prometheus.MustNewConstMetric(jobsDesc, prometheus.GaugeValue, float64(s.jobs), queue, state, label)
				ch <- prometheus.MustNewConstMetric(highAttemptDesc, prometheus.GaugeValue, float64(s.highAttempts), queue, state, label)
			}
			ch <- prometheus.MustNewConstMetric(oldestAvailableDesc, prometheus.GaugeValue,
				byGroup[group{queue, "available", priority}].oldestAvailableSeconds, queue, label)
		}
	}
}

// start polls while ctx, River's leadership term, lasts.
func (c *queueCollector) start(ctx context.Context) {
	c.mu.Lock()
	c.term++
	term := c.term
	c.active, c.collected, c.success = true, false, false
	c.statuses, c.backlog = nil, nil
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			if c.term == term {
				c.active = false
				c.statuses, c.backlog = nil, nil
			}
			c.mu.Unlock()
		}()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			c.poll(ctx, term)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (c *queueCollector) poll(ctx context.Context, term uint64) {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	statuses, err := c.snapshot(queryCtx)
	var backlog map[string]int64
	if err == nil {
		backlog, err = c.tenantBacklog(queryCtx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || c.term != term {
		return
	}
	c.collected, c.success = true, err == nil
	c.statuses, c.backlog = nil, nil
	if err == nil {
		c.statuses, c.backlog = statuses, backlog
	}
}

// snapshot excludes terminal jobs so retained history cannot make a scrape
// increasingly costly.
func (c *queueCollector) snapshot(ctx context.Context) ([]jobStatus, error) {
	rows, err := c.pool.Query(ctx, `
SELECT queue, state::text, priority, count(*), count(*) FILTER (WHERE attempt >= 3),
  coalesce(greatest(0, extract(epoch FROM now() - min(scheduled_at)
    FILTER (WHERE state = 'available' AND scheduled_at <= now()))), 0)::float8
FROM `+c.jobs+`
WHERE queue IN ('media_ingest', 'media_derivatives')
  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
GROUP BY queue, state, priority`)
	if err != nil {
		return nil, fmt.Errorf("media/worker: queue snapshot: %w", err)
	}
	defer rows.Close()
	var out []jobStatus
	for rows.Next() {
		var s jobStatus
		if err := rows.Scan(&s.queue, &s.state, &s.priority, &s.jobs, &s.highAttempts, &s.oldestAvailableSeconds); err != nil {
			return nil, fmt.Errorf("media/worker: queue snapshot: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// tenantBacklog counts derivative jobs waiting to run by the tenant River stored.
func (c *queueCollector) tenantBacklog(ctx context.Context) (map[string]int64, error) {
	rows, err := c.pool.Query(ctx, `
SELECT args->'ref'->>'tenant_id', count(*)
FROM `+c.jobs+`
WHERE queue = 'media_derivatives'
  AND state IN ('available', 'pending', 'retryable', 'scheduled')
  AND nullif(args->'ref'->>'tenant_id', '') IS NOT NULL
GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("media/worker: tenant backlog: %w", err)
	}
	defer rows.Close()
	backlog := map[string]int64{}
	for rows.Next() {
		var tenant string
		var n int64
		if err := rows.Scan(&tenant, &n); err != nil {
			return nil, fmt.Errorf("media/worker: tenant backlog: %w", err)
		}
		backlog[tenant] = n
	}
	return backlog, rows.Err()
}
