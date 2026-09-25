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
	attempted := time.Now().Add(-time.Second)
	job := &rivertype.JobRow{Queue: workqueue.VideoQueue, Attempt: 3, AttemptedAt: &attempted}
	for _, result := range []error{nil, river.JobSnooze(time.Minute), river.JobCancel(errors.New("cancelled")), errors.New("failed")} {
		if got := metrics.WorkEnd(context.Background(), job, result); got != result {
			t.Fatalf("WorkEnd changed the job result: %v", got)
		}
	}
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
		`contentkit_media_job_attempts_total{attempt="3+",outcome="success",queue="media_video"} 1`,
		`contentkit_media_job_attempts_total{attempt="3+",outcome="snoozed",queue="media_video"} 1`,
		`contentkit_media_job_attempts_total{attempt="3+",outcome="cancelled",queue="media_video"} 1`,
		`contentkit_media_job_attempts_total{attempt="3+",outcome="error",queue="media_video"} 1`,
		`contentkit_media_video_encode_cpu_seconds_total{outcome="success",source_class="hd"} 1.25`,
		`contentkit_media_video_encode_cpu_seconds_total{outcome="error",source_class="hd"} 0.5`,
		`contentkit_media_video_encoded_output_seconds_total{source_class="hd"} 2`,
	} {
		if !strings.Contains(rec.Body.String(), metric) {
			t.Errorf("missing %q in metrics", metric)
		}
	}
}
