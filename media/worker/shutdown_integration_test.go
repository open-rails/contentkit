package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type shutdownProbeArgs struct{}

func (shutdownProbeArgs) Kind() string { return "worker_shutdown_probe" }

type shutdownProbe struct {
	river.WorkerDefaults[shutdownProbeArgs]
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (w *shutdownProbe) Work(ctx context.Context, _ *river.Job[shutdownProbeArgs]) error {
	close(w.started)
	<-ctx.Done()
	close(w.canceled)
	<-w.release
	return ctx.Err()
}

func TestStopJobsReportsUnfinishedCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	probe := &shutdownProbe{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, probe)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema, Workers: workers, Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}}, JobTimeout: time.Hour, SoftStopTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-probe.release:
		default:
			close(probe.release)
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := client.StopAndCancel(cleanup); err != nil {
			t.Error(err)
		}
	})
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := client.Insert(ctx, shutdownProbeArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-probe.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	err = stopJobs(client, 5*time.Millisecond, 25*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished job reported as stopped: %v", err)
	}
	select {
	case <-probe.canceled:
	case <-time.After(time.Second):
		t.Fatal("hard stop did not interrupt job")
	}
	select {
	case <-client.Stopped():
		t.Fatal("client stopped before job unwound")
	default:
	}
	if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("pool must remain available during cancellation: %v", err)
	}
	close(probe.release)
	if err := stopJobs(client, time.Second, time.Second); err != nil {
		t.Fatalf("join completed handoff: %v", err)
	}
	var state string
	var attempt int
	if err := pool.QueryRow(ctx, "SELECT state,attempt FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE id=$1", job.Job.ID).Scan(&state, &attempt); err != nil {
		t.Fatal(err)
	}
	if state != "available" || attempt != 0 {
		t.Fatalf("job was not returned for retry: state=%s attempt=%d", state, attempt)
	}
}
