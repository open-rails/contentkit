package workqueue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/open-rails/contentkit/internal/pgtest"
)

func TestCollector(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+jobs(schema)+`
  (kind, args, max_attempts, queue, state, priority, attempt, scheduled_at)
VALUES ('contentkit_media_video', '{}', 5, 'media_video', 'available', 1, 3, $1)`, time.Now().Add(-time.Hour)); err != nil {
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
		"contentkit_media_queue_collection_success 1",
		`contentkit_media_jobs{priority="1",queue="media_video",state="available"} 1`,
		`contentkit_media_high_attempt_jobs{priority="1",queue="media_video",state="available"} 1`,
		`contentkit_media_jobs{priority="1",queue="media_audio",state="available"} 0`,
	} {
		if !strings.Contains(rec.Body.String(), metric) {
			t.Errorf("missing %q in metrics", metric)
		}
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
	if !strings.Contains(rec.Body.String(), "contentkit_media_queue_collection_success 0") {
		t.Fatal("database failure was not exposed")
	}
	if strings.Contains(rec.Body.String(), "contentkit_media_jobs{") {
		t.Fatal("database failure was reported as an empty queue")
	}
}
