package video

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
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
RETURNING id`, (workqueue.VideoArgs{}).Kind(), tenant, state, delay.Seconds()).Scan(&id)
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

type replanArgs struct {
	Ref     contentref.ContentRef `json:"ref"`
	Done    int                   `json:"done"`
	Rechunk bool                  `json:"rechunk"`
}

func (replanArgs) Kind() string { return "test_replan" }

// A rung adopted from existing outputs has no chunks; a forced regeneration
// reopening it plans them, and a reopened run keeps the chunks it has.
func TestReopenedRunGetsChunks(t *testing.T) {
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	c := WorkerConfig{Pool: pool, Schema: schema}
	stages := []rung{{n: 240, w: 426, h: 240}, {n: 480, w: 854, h: 480}}
	bounds := [][][2]int64{{{0, 16000}, {16000, 32000}, {32000, 40000}}, {{0, 40000}}}
	rechunked := [][][2]int64{{{0, 8000}, {8000, 16000}, {16000, 24000}, {24000, 32000}, {32000, 40000}}, bounds[1]}
	// insertRuns takes its River client from the job context, as in planning.
	workers := river.NewWorkers()
	river.AddWorker(workers, river.WorkFunc(func(ctx context.Context, job *river.Job[replanArgs]) error {
		b := bounds
		if job.Args.Rechunk {
			b = rechunked
		}
		return c.insertRuns(ctx, job.Args.Ref, "source.mkv", "blob", "key", "etag", "hls:fp", media.VideoJobClass("ordinary"),
			probeResult{}, stages, b, job.Args.Done, "")
	}))
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema, Workers: workers, SkipUnknownJobCheck: true,
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}}, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Stop(ctx)
	// plan runs n planners at once.
	plan := func(args replanArgs, n int) {
		t.Helper()
		for range n {
			if _, err := client.Insert(ctx, args, &river.InsertOpts{MaxAttempts: 1}); err != nil {
				t.Fatal(err)
			}
		}
		for range n {
			select {
			case ev := <-events:
				if ev.Kind == river.EventKindJobFailed {
					t.Fatalf("planning failed: %+v", ev.Job.Errors)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("planning did not finish")
			}
		}
	}
	type run struct {
		state                string
		chunks, queued, jobs int
	}
	check := func(id string, rung int, want run) {
		t.Helper()
		var got run
		if err := pool.QueryRow(ctx, `SELECT r.state,
  (SELECT count(*) FROM `+c.chunkTable()+` ch WHERE ch.run_id = r.id),
  (SELECT count(*) FROM `+c.chunkTable()+` ch WHERE ch.run_id = r.id AND ch.state = 'queued'),
  (SELECT count(*) FROM `+c.jobTable()+` j WHERE j.args->>'run_id' = r.id::text AND j.state = 'available')
FROM `+c.runTable()+` r WHERE r.ref->>'content_id' = $1 AND r.rung = $2`, id, rung).Scan(&got.state, &got.chunks, &got.queued, &got.jobs); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rung %d: %+v, want %+v", id, rung, got, want)
		}
	}
	complete := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE `+c.runTable()+` SET state = 'complete' WHERE ref->>'content_id' = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	adopted := replanArgs{Ref: contentref.New("tenant", "video", "adopted"), Done: 1}
	plan(adopted, 1)
	check("adopted", 240, run{state: "complete"})
	check("adopted", 480, run{state: "planned", chunks: 1})
	// Once the upper rung is done, a forced regeneration replans every rung.
	complete("adopted")
	adopted.Done = 0
	plan(adopted, 1)
	check("adopted", 240, run{state: "planned", chunks: 3})
	// A replay creates no additional chunks or child jobs.
	plan(adopted, 1)
	check("adopted", 240, run{state: "planned", chunks: 3})

	// A cancelled run is retried with its own chunks, not ones planned anew.
	if _, err := pool.Exec(ctx, `UPDATE `+c.runTable()+` SET state = 'cancelled' WHERE rung = 240`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+c.jobTable()+` SET state = 'cancelled', finalized_at = now() WHERE args ? 'run_id'`); err != nil {
		t.Fatal(err)
	}
	adopted.Rechunk = true
	plan(adopted, 1)
	check("adopted", 240, run{state: "planned", chunks: 3})

	// Competing planners reopening an adopted run plan its chunks once.
	racing := replanArgs{Ref: contentref.New("tenant", "video", "racing"), Done: 1}
	plan(racing, 1)
	complete("racing")
	racing.Done = 0
	// Hold the run so both planners reach the same reopen concurrently.
	// Without locking the state read they both see complete and no chunks,
	// then block at the chunk foreign key instead.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM `+c.runTable()+` WHERE ref->>'content_id' = 'racing' AND rung = 240 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	for _, rechunk := range []bool{false, true} {
		racing.Rechunk = rechunk
		if _, err := client.Insert(ctx, racing, &river.InsertOpts{MaxAttempts: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE wait_event_type = 'Lock' AND position($1 in query) > 0`, schema).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("both planners did not reach the blocked run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case ev := <-events:
			if ev.Kind == river.EventKindJobFailed {
				t.Fatalf("competing planner failed: %+v", ev.Job.Errors)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	rows, err := pool.Query(ctx, `SELECT ch.start_ms, ch.end_ms FROM `+c.chunkTable()+` ch
JOIN `+c.runTable()+` r ON r.id = ch.run_id WHERE r.ref->>'content_id' = 'racing' AND r.rung = 240 ORDER BY ch.ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]int64, error) {
		var bound [2]int64
		err := row.Scan(&bound[0], &bound[1])
		return bound, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, bounds[0]) && !slices.Equal(got, rechunked[0]) {
		t.Fatalf("competing planners mixed chunk bounds: %v", got)
	}
	check("racing", 240, run{state: "planned", chunks: len(got)})
}
