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
	aged, err := client.Insert(ctx, workqueue.VideoArgs{Ref: ref("tenant-a", "video")},
		&river.InsertOpts{Queue: workqueue.VideoQueue, Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []struct {
		args river.JobArgs
		opts river.InsertOpts
	}{
		{workqueue.VideoArgs{Ref: ref("tenant-a", "video")}, river.InsertOpts{Queue: workqueue.VideoQueue, Priority: 2}},
		{workqueue.VideoArgs{Ref: ref("tenant-b", "video")}, river.InsertOpts{Queue: workqueue.VideoQueue, Priority: 1, Pending: true}},
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
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, metric := range []string{
		"media_video_queue_collection_success 1",
		`media_video_jobs{priority="1",queue="media_video",state="available"} 1`,
		`media_video_jobs{priority="2",queue="media_video",state="available"} 1`,
		`media_video_jobs{priority="1",queue="media_video",state="pending"} 1`,
		`media_video_jobs{priority="1",queue="media_image",state="available"} 1`,
		`media_video_jobs{priority="4",queue="media_audio",state="available"} 1`,
		`media_video_high_attempt_jobs{priority="1",queue="media_video",state="available"} 1`,
		`media_video_tenant_backlog{tenant="tenant-a"} 2`,
		`media_video_tenant_backlog{tenant="tenant-b"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), metric) {
			t.Errorf("missing %q in metrics", metric)
		}
	}
	oldest := regexp.MustCompile(`media_video_oldest_available_seconds\{priority="1",queue="media_video"\} ([0-9.]+)`).FindStringSubmatch(rec.Body.String())
	if len(oldest) != 2 {
		t.Fatal("missing oldest video job age")
	}
	seconds, err := strconv.ParseFloat(oldest[1], 64)
	if err != nil || seconds < 3500 || seconds > 3700 {
		t.Fatalf("oldest video job age = %q, want about 3600 seconds", oldest[1])
	}
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
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "media_video_queue_collection_success 0") {
		t.Fatal("database failure was not exposed")
	}
	if strings.Contains(rec.Body.String(), "media_video_jobs{") {
		t.Fatal("database failure was reported as an empty queue")
	}
}
