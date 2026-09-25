package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media/workqueue"
)

func TestCollector(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	ref := func(tenant, kind string) contentref.ContentRef {
		return contentref.New(tenant, kind, contentref.NewID())
	}
	aged, err := client.Insert(ctx, workqueue.VideoPlanArgs{Ref: ref("tenant-a", "video")},
		&river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []struct {
		args river.JobArgs
		opts river.InsertOpts
	}{
		{workqueue.VideoPlanArgs{Ref: ref("tenant-a", "video")}, river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: 2}},
		{workqueue.VideoPlanArgs{Ref: ref("tenant-b", "video")}, river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: 1, Pending: true}},
		{workqueue.VideoChunkArgs{Ref: ref("tenant-c", "video"), RunID: "run-c"}, river.InsertOpts{Queue: workqueue.VideoEncodeQueue, Priority: 3}},
		{workqueue.ImageArgs{Ref: ref("tenant-b", "gallery")}, river.InsertOpts{Queue: workqueue.ImageQueue, Priority: 1}},
		{workqueue.AudioArgs{Ref: ref("tenant-c", "audio")}, river.InsertOpts{Queue: workqueue.AudioQueue, Priority: 4}},
	} {
		if _, err := client.Insert(ctx, job.args, &job.opts); err != nil {
			t.Fatal(err)
		}
	}
	jobs := pgx.Identifier{schema, "river_job"}.Sanitize()
	if _, err := pool.Exec(ctx, `UPDATE `+jobs+` SET attempt = 3, scheduled_at = $2 WHERE id = $1`,
		aged.Job.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	collector, err := NewCollector(pool, schema)
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	if err := registry.Register(collector); err != nil {
		t.Fatal(err)
	}
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "media_video_queue_collection_success") {
		t.Fatal("queue metrics appeared before River elected a leader")
	}
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &metricsWorker{}); err != nil {
		t.Fatal(err)
	}
	leader, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema:  schema,
		Queues:  map[string]river.QueueConfig{"metrics_test": {MaxWorkers: 1}},
		Workers: workers,
		Hooks:   []rivertype.Hook{collector.LeaderHook()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := leader.StopAndCancel(stopCtx); err != nil {
			t.Error(err)
		}
	})
	var body string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("metrics status = %d: %s", rec.Code, rec.Body.String())
		}
		body = rec.Body.String()
		if strings.Contains(body, "media_video_queue_collection_success 1") {
			break
		}
	}
	if !strings.Contains(body, "media_video_queue_collection_success 1") {
		t.Fatalf("leader did not poll queue: %s", body)
	}
	for _, metric := range []string{
		`media_video_jobs{priority="1",queue="media_video_light",state="available"} 1`,
		`media_video_jobs{priority="2",queue="media_video_light",state="available"} 1`,
		`media_video_jobs{priority="1",queue="media_video_light",state="pending"} 1`,
		`media_video_jobs{priority="3",queue="media_video_encode",state="available"} 1`,
		`media_video_jobs{priority="1",queue="media_image",state="available"} 1`,
		`media_video_jobs{priority="4",queue="media_audio",state="available"} 1`,
		`media_video_high_attempt_jobs{priority="1",queue="media_video_light",state="available"} 1`,
		`media_video_tenant_backlog{tenant="tenant-a"} 2`,
		`media_video_tenant_backlog{tenant="tenant-b"} 1`,
		`media_video_tenant_backlog{tenant="tenant-c"} 1`,
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("missing %q in metrics", metric)
		}
	}
	oldest := regexp.MustCompile(`media_video_oldest_available_seconds\{priority="1",queue="media_video_light"\} ([0-9.]+)`).FindStringSubmatch(body)
	if len(oldest) != 2 {
		t.Fatal("missing oldest video job age")
	}
	seconds, err := strconv.ParseFloat(oldest[1], 64)
	if err != nil || seconds < 3500 || seconds > 3700 {
		t.Fatalf("oldest video job age = %q, want about 3600 seconds", oldest[1])
	}
	if _, err := client.Insert(ctx, workqueue.VideoPlanArgs{Ref: ref("tenant-a", "video")},
		&river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: 1}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `media_video_jobs{priority="1",queue="media_video_light",state="available"} 1`) {
		t.Fatal("scrape queried the queue instead of serving the leader's snapshot")
	}
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := leader.StopAndCancel(stopCtx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if !strings.Contains(rec.Body.String(), "media_video_queue_collection_success") {
			return
		}
	}
	t.Fatal("queue metrics remained after River leadership stopped")
}

func TestCollectorReportsDatabaseFailure(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	collector, err := NewCollector(pool, "missing_media_worker")
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	if err := registry.Register(collector); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector.start(ctx)
	var body string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		rec := httptest.NewRecorder()
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("metrics status = %d: %s", rec.Code, rec.Body.String())
		}
		body = rec.Body.String()
		if strings.Contains(body, "media_video_queue_collection_success 0") {
			break
		}
	}
	if !strings.Contains(body, "media_video_queue_collection_success 0") {
		t.Fatal("database failure was not exposed")
	}
	if strings.Contains(body, "media_video_jobs{") {
		t.Fatal("database failure was reported as an empty queue")
	}
}

type metricsJob struct{}

func (metricsJob) Kind() string { return "metrics_test" }

type metricsWorker struct {
	river.WorkerDefaults[metricsJob]
}

func (*metricsWorker) Work(context.Context, *river.Job[metricsJob]) error { return nil }
