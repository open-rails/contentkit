package worker_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media/workqueue"
)

type readinessAttempt struct {
	id                int64
	attempt, failures int
}

type failAfterImage struct {
	river.HookDefaults
	armed, recovered atomic.Bool
	completed        chan readinessAttempt
}

// This runs before the stock readiness hook. The image has finished; stop
// further attempts while the test observes the readiness hook's outcome.
func (h *failAfterImage) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) error {
	if err != nil || job.Kind != (workqueue.ImageArgs{}).Kind() || !h.armed.CompareAndSwap(false, true) {
		return err
	}
	if err := river.ClientFromContext[pgx.Tx](ctx).QueuePause(ctx, workqueue.ImageQueue, nil); err != nil {
		h.armed.Store(false)
		return err
	}
	h.completed <- readinessAttempt{job.ID, job.Attempt, len(job.Errors)}
	return nil
}

// A successful conversion followed by an S3 outage must retain its output and
// snooze through the real River hook context. One bad manifest on an otherwise
// healthy bucket instead spends an attempt; it is not a bucket outage.
func TestReadinessEndOutagePolicy(t *testing.T) {
	for _, bucketDown := range []bool{true, false} {
		name := "bad_manifest_on_healthy_bucket"
		if bucketDown {
			name = "bucket_outage"
		}
		t.Run(name, func(t *testing.T) {
			fault := &failAfterImage{completed: make(chan readinessAttempt, 1)}
			var healthChecks atomic.Int64
			h := faultyWorker(t, func(w http.ResponseWriter, r *http.Request) bool {
				if !fault.armed.Load() || fault.recovered.Load() {
					return false
				}
				if r.URL.Query().Get("list-type") == "2" && strings.Contains(r.URL.Query().Get("prefix"), "_health/") {
					healthChecks.Add(1)
				}
				if !bucketDown && !(r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest.json")) {
					return false
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `<Error><Code>ServiceUnavailable</Code></Error>`)
				return true
			}, fault)
			events, unsubscribe := h.worker.Client().Subscribe(river.EventKindJobSnoozed, river.EventKindJobFailed)
			defer unsubscribe()
			ref := h.ref(t, "gallery")
			h.upload(t, ref, "originals/001.png", "image/png", pngImage(t, 60, 80, 91))
			var before readinessAttempt
			select {
			case before = <-fault.completed:
			case <-time.After(2 * time.Minute):
				t.Fatal("image did not reach its readiness hook")
			}
			output, ok := h.file(ref, "thumb/001.webp")
			if !ok || output.Blob == "" {
				t.Fatal("conversion output was not durable before the hook fault")
			}
			var event *river.Event
			deadline := time.After(time.Minute)
		awaitOutcome:
			for {
				select {
				case candidate := <-events:
					if candidate.Job.ID == before.id {
						event = candidate
						break awaitOutcome
					}
				case <-deadline:
					t.Fatal("readiness hook did not finish")
				}
			}
			var attempt, failures, outages int
			if err := h.pool.QueryRow(t.Context(), `SELECT attempt,coalesce(cardinality(errors),0),coalesce((metadata->>'contentkit_outage_snoozes')::int,0)
    FROM `+h.workers+`.river_job WHERE id=$1`, before.id).Scan(&attempt, &failures, &outages); err != nil {
				t.Fatal(err)
			}
			if bucketDown {
				if event.Kind != river.EventKindJobSnoozed || attempt != before.attempt-1 || failures != before.failures || outages != 1 {
					t.Fatalf("bucket outage: event=%s attempt=%d failures=%d outages=%d; before=%+v", event.Kind, attempt, failures, outages, before)
				}
			} else if event.Kind != river.EventKindJobFailed || attempt != before.attempt || failures != before.failures+1 || outages != 0 {
				t.Fatalf("bad manifest: event=%s attempt=%d failures=%d outages=%d; before=%+v", event.Kind, attempt, failures, outages, before)
			}
			if healthChecks.Load() == 0 {
				t.Fatal("readiness error did not trigger a fresh bucket check")
			}
			fault.recovered.Store(true)
			if _, err := h.worker.Client().JobRetry(t.Context(), before.id); err != nil {
				t.Fatal(err)
			}
			if err := h.worker.Client().QueueResume(t.Context(), workqueue.ImageQueue, nil); err != nil {
				t.Fatal(err)
			}
			eventually(t, "ItemReady after the readiness hook recovers", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.Ready() })
			after, ok := h.file(ref, "thumb/001.webp")
			if !ok || after.Blob != output.Blob {
				t.Fatal("readiness retry replaced a completed conversion output")
			}
		})
	}
}
