package workqueue

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
)

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	other := pgtest.EmptySchema(t, ctx, pool)
	for _, name := range []string{schema, other} {
		if err := Migrate(ctx, pool, name); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	insert := func(schema, queue, state string, priority, attempt int, scheduled time.Time) {
		t.Helper()
		var finalized *time.Time
		if state == "cancelled" || state == "completed" || state == "discarded" {
			finalized = &now
		}
		_, err := pool.Exec(ctx, `INSERT INTO `+jobs(schema)+`
  (kind, args, max_attempts, queue, state, priority, attempt, scheduled_at, finalized_at)
VALUES ('contentkit_media_video', '{}', 5, $1, $2, $3, $4, $5, $6)`,
			queue, state, priority, attempt, scheduled, finalized)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(schema, VideoQueue, "available", 1, 3, now.Add(-2*time.Hour))
	insert(schema, VideoQueue, "available", 1, 0, now.Add(-10*time.Minute))
	insert(schema, VideoQueue, "available", 2, 4, now.Add(time.Hour))
	insert(schema, AudioQueue, "running", 1, 2, now)
	insert(schema, ImageQueue, "retryable", 1, 3, now)
	insert(schema, ImageQueue, "scheduled", 4, 0, now.Add(time.Hour))
	insert(schema, VideoQueue, "completed", 1, 5, now.Add(-3*time.Hour))
	insert(other, VideoQueue, "available", 1, 0, now.Add(-24*time.Hour))

	statuses, err := Snapshot(ctx, pool, schema)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 5 {
		t.Fatalf("got %d groups, want 5: %+v", len(statuses), statuses)
	}
	byGroup := make(map[string]JobStatus, len(statuses))
	for _, status := range statuses {
		key := status.Queue + "/" + status.State + "/" + strconv.Itoa(status.Priority)
		byGroup[key] = status
	}
	for key, want := range map[string]struct{ jobs, high int64 }{
		"media_video/available/1": {2, 1},
		"media_video/available/2": {1, 1},
		"media_audio/running/1":   {1, 0},
		"media_image/retryable/1": {1, 1},
		"media_image/scheduled/4": {1, 0},
	} {
		got, ok := byGroup[key]
		if !ok || got.Jobs != want.jobs || got.HighAttemptJobs != want.high {
			t.Errorf("%s: got %+v (present %t), want %d jobs and %d high attempts", key, got, ok, want.jobs, want.high)
		}
	}
	if age := byGroup["media_video/available/1"].OldestAvailableSeconds; age < (2*time.Hour-15*time.Second).Seconds() || age > (2*time.Hour+15*time.Second).Seconds() {
		t.Errorf("oldest runnable age = %.0f seconds, want about two hours", age)
	}
	for _, key := range []string{"media_video/available/2", "media_audio/running/1", "media_image/retryable/1", "media_image/scheduled/4"} {
		if age := byGroup[key].OldestAvailableSeconds; age != 0 {
			t.Errorf("%s: age = %.0f, want zero", key, age)
		}
	}
}

func TestSnapshotErrors(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	if _, err := Snapshot(ctx, pool, "bad-schema"); err == nil {
		t.Fatal("invalid schema was accepted")
	}
	if _, err := Snapshot(ctx, pool, "missing_media_worker"); err == nil {
		t.Fatal("missing schema was reported as an empty queue")
	}
}
