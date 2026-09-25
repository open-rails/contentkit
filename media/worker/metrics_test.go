package worker_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
)

func TestMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := worker.NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	job := &rivertype.JobRow{Queue: workqueue.VideoLightQueue, Attempt: 3}
	for _, result := range []error{nil, river.JobSnooze(time.Minute), river.JobCancel(errors.New("cancelled")), errors.New("failed")} {
		if got := metrics.Work(context.Background(), job, func(context.Context) error { return result }); got != result {
			t.Fatalf("Work changed the job result: %v", got)
		}
	}
	remote, cancel := context.WithCancelCause(context.Background())
	cancel(river.ErrJobCancelledRemotely)
	if got := metrics.Work(remote, job, func(context.Context) error { return context.Canceled }); got != context.Canceled {
		t.Fatalf("Work changed remote cancellation: %v", got)
	}
	func() {
		defer func() {
			if got := recover(); got != "panic before WorkEnd" {
				t.Fatalf("Work did not propagate panic: %v", got)
			}
		}()
		_ = metrics.Work(context.Background(), job, func(context.Context) error { panic("panic before WorkEnd") })
	}()
	metrics.ObserveEncode(video.EncodeObservation{SourceClass: "hd", Duration: 3 * time.Second,
		CPU: 1250 * time.Millisecond, OutputSeconds: 2, Succeeded: true})
	metrics.ObserveEncode(video.EncodeObservation{SourceClass: "hd", Duration: time.Second,
		CPU: 500 * time.Millisecond})

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, metric := range []string{
		`media_video_attempt_bucket{outcome="success",queue="media_video_light",le="3"} 1`,
		`media_video_attempt_count{outcome="snoozed",queue="media_video_light"} 1`,
		`media_video_attempt_count{outcome="cancelled",queue="media_video_light"} 2`,
		`media_video_attempt_count{outcome="error",queue="media_video_light"} 2`,
		`media_video_encode_cpu_seconds_total{class="hd",outcome="success"} 1.25`,
		`media_video_encode_cpu_seconds_total{class="hd",outcome="error"} 0.5`,
		`media_video_encoded_output_seconds_total{class="hd"} 2`,
		`media_video_core_seconds_per_output_second_count{class="hd"} 1`,
		`media_video_core_seconds_per_output_second_sum{class="hd"} 0.625`,
	} {
		if !strings.Contains(rec.Body.String(), metric) {
			t.Errorf("missing %q in metrics", metric)
		}
	}
}

type blockingArgs struct{}

func (blockingArgs) Kind() string { return "metrics_blocking" }

type blockingWorker struct {
	river.WorkerDefaults[blockingArgs]
	started chan struct{}
}

func (w *blockingWorker) Work(ctx context.Context, _ *river.Job[blockingArgs]) error {
	close(w.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestMetricsRemoteCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	metrics, err := worker.NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	started := make(chan struct{})
	if err := river.AddWorkerSafely(workers, &blockingWorker{started: started}); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema, Workers: workers,
		Queues:     map[string]river.QueueConfig{workqueue.VideoLightQueue: {MaxWorkers: 1}},
		Middleware: []rivertype.Middleware{metrics}, FetchPollInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := client.Subscribe(river.EventKindJobCancelled)
	defer unsubscribe()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := client.StopAndCancel(stopCtx); err != nil {
			t.Error(err)
		}
	})
	inserted, err := client.Insert(ctx, blockingArgs{}, &river.InsertOpts{Queue: workqueue.VideoLightQueue})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("River did not start the job")
	}
	// River's notifier starts asynchronously after the client starts.
	time.Sleep(500 * time.Millisecond)
	if _, err := client.JobCancel(ctx, inserted.Job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("River did not cancel the running job")
	}
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `media_video_attempt_count{outcome="cancelled",queue="media_video_light"} 1`) {
		t.Fatalf("remote cancellation was not counted as cancelled: %s", rec.Body.String())
	}
}
