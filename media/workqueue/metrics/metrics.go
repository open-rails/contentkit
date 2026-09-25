// Package metrics exports the host's media worker queue health. It is kept
// separate from workqueue so a host can enqueue without linking Prometheus.
package metrics

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

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

var queues = [...]string{workqueue.ImageQueue, workqueue.VideoQueue, workqueue.AudioQueue}
var activeStates = [...]string{"available", "pending", "retryable", "running", "scheduled"}

// Collector reads queue health from the host database at scrape time. Hosts
// register it on an always-on process, not only on workers that scale to zero.
type Collector struct {
	pool   *pgxpool.Pool
	schema string
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	statuses, err := workqueue.Snapshot(ctx, c.pool, c.schema)
	var backlog map[string]int64
	if err == nil {
		backlog, err = workqueue.TenantBacklog(ctx, c.pool, c.schema)
	}
	if err != nil {
		ch <- prometheus.MustNewConstMetric(collectionSuccessDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(collectionSuccessDesc, prometheus.GaugeValue, 1)
	for tenant, jobs := range backlog {
		ch <- prometheus.MustNewConstMetric(tenantBacklogDesc, prometheus.GaugeValue, float64(jobs), tenant)
	}

	type group struct {
		queue, state string
		priority     int
	}
	byGroup := make(map[group]workqueue.JobStatus, len(statuses))
	for _, status := range statuses {
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
