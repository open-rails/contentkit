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
	"github.com/riverqueue/river/rivertype"

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
	job := &rivertype.JobRow{Queue: workqueue.VideoQueue, Attempt: 3}
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
		`media_video_attempt_bucket{outcome="success",queue="media_video",le="3"} 1`,
		`media_video_attempt_count{outcome="snoozed",queue="media_video"} 1`,
		`media_video_attempt_count{outcome="cancelled",queue="media_video"} 2`,
		`media_video_attempt_count{outcome="error",queue="media_video"} 2`,
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
