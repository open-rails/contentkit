package video

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media/workqueue"
)

func TestChunkBoundsRejectsExcessiveDuration(t *testing.T) {
	rungs := []rung{{n: 480, w: 854, h: 480}}
	short, err := chunkBounds(plan{duration: 9, fps: 30}, rungs, 2, 5*time.Minute)
	if err != nil || len(short) != 1 {
		t.Fatalf("ordinary video chunks = %d, err %v", len(short), err)
	}
	if _, err := chunkBounds(plan{duration: 1e12, fps: 30}, rungs, 2, 5*time.Minute); err == nil {
		t.Fatal("unbounded video duration was accepted")
	}
}

func TestTenantShareIgnoresFutureJobs(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	w := WorkerConfig{Pool: pool, Schema: schema}
	insert := func(tenant, state string, delay time.Duration) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO `+w.jobTable()+`
  (kind, args, state, scheduled_at, max_attempts)
VALUES ($1, jsonb_build_object('ref', jsonb_build_object('tenant_id', $2::text)), $3, now() + $4 * interval '1 second', 5)
RETURNING id`, (workqueue.VideoChunkArgs{}).Kind(), tenant, state, delay.Seconds()).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert("own", "running", 0)
	insert("own", "running", 0)
	other := insert("other", "scheduled", time.Hour)
	check := func(want bool) {
		t.Helper()
		got, err := w.overTenantShare(ctx, "own")
		if err != nil || got != want {
			t.Fatalf("over share = %t, err %v, want %t", got, err, want)
		}
	}
	check(false)
	if _, err := pool.Exec(ctx, `UPDATE `+w.jobTable()+` SET scheduled_at = now() - interval '1 second' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	check(true)
}
