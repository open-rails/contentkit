// Package metrics exports the host's media worker queue health. It is kept
// separate from workqueue so a host can enqueue without linking Prometheus.
package metrics

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

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
	tenantBacklogDesc = prometheus.NewDesc("media_video_tenant_backlog", "Video jobs waiting to run by tenant.",
		[]string{"tenant"}, nil)
	collectionSuccessDesc = prometheus.NewDesc("media_video_queue_collection_success", "Whether the current queue snapshot was read successfully (1) or failed (0).", nil, nil)
)

var queues = [...]string{workqueue.ImageQueue, workqueue.VideoLightQueue, workqueue.VideoEncodeQueue, workqueue.AudioQueue}
var activeStates = [...]string{"available", "pending", "retryable", "running", "scheduled"}

// Collector serves the latest queue snapshot read by an always-on host's River
// leader. A one-shot media worker can scale to zero and cannot expose backlog.
type Collector struct {
	pool      *pgxpool.Pool
	schema    string
	mu        sync.RWMutex
	term      uint64
	active    bool
	collected bool
	success   bool
	statuses  []workqueue.JobStatus
	backlog   map[string]int64
}

var _ prometheus.Collector = (*Collector)(nil)

// NewCollector returns queue metrics for one worker schema.
func NewCollector(pool *pgxpool.Pool, schema string) (*Collector, error) {
	if pool == nil {
		return nil, errors.New("media/workqueue/metrics: Collector needs a pool")
	}
	if err := workqueue.ValidSchema(schema); err != nil {
		return nil, err
	}
	return &Collector{pool: pool, schema: schema}, nil
}

func (*Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- jobsDesc
	ch <- highAttemptDesc
	ch <- oldestAvailableDesc
	ch <- tenantBacklogDesc
	ch <- collectionSuccessDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
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
	byGroup := make(map[group]workqueue.JobStatus, len(c.statuses))
	for _, status := range c.statuses {
		byGroup[group{status.Queue, status.State, status.Priority}] = status
	}
	for _, queue := range queues {
		for priority := 1; priority <= 4; priority++ {
			label := strconv.Itoa(priority)
			for _, state := range activeStates {
				status := byGroup[group{queue, state, priority}]
				ch <- prometheus.MustNewConstMetric(jobsDesc, prometheus.GaugeValue, float64(status.Jobs), queue, state, label)
				ch <- prometheus.MustNewConstMetric(highAttemptDesc, prometheus.GaugeValue, float64(status.HighAttemptJobs), queue, state, label)
			}
			ch <- prometheus.MustNewConstMetric(oldestAvailableDesc, prometheus.GaugeValue,
				byGroup[group{queue, "available", priority}].OldestAvailableSeconds, queue, label)
		}
	}
}

// LeaderHook starts queue polling only while this River client holds leadership.
func (c *Collector) LeaderHook() rivertype.Hook {
	return river.HookPeriodicJobsStartFunc(func(ctx context.Context, _ *rivertype.HookPeriodicJobsStartParams) error {
		c.start(ctx)
		return nil
	})
}

func (c *Collector) start(ctx context.Context) {
	c.mu.Lock()
	c.term++
	term := c.term
	c.active, c.collected, c.success = true, false, false
	c.statuses, c.backlog = nil, nil
	c.mu.Unlock()
	go c.pollLeader(ctx, term)
}

func (c *Collector) pollLeader(ctx context.Context, term uint64) {
	defer func() {
		c.mu.Lock()
		if c.term == term {
			c.active = false
			c.statuses, c.backlog = nil, nil
		}
		c.mu.Unlock()
	}()

	c.poll(ctx, term)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx, term)
		}
	}
}

func (c *Collector) poll(ctx context.Context, term uint64) {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	statuses, err := workqueue.Snapshot(queryCtx, c.pool, c.schema)
	var backlog map[string]int64
	if err == nil {
		backlog, err = workqueue.TenantBacklog(queryCtx, c.pool, c.schema)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || c.term != term {
		return
	}
	c.collected, c.success = true, err == nil
	if err == nil {
		c.statuses, c.backlog = statuses, backlog
	} else {
		c.statuses, c.backlog = nil, nil
	}
}
