package media

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/internal/pgtest"
)

// The idle periodic workers need no object access beyond an empty upgrade scan.
type idleCleanupStore struct{ Store }

func (idleCleanupStore) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{ConditionalPut: true}, nil
}
func (idleCleanupStore) List(context.Context, string) iter.Seq2[Object, error] {
	return func(func(Object, error) bool) {}
}

type cleanupStartupProbeArgs struct{}

func (cleanupStartupProbeArgs) Kind() string { return "cleanup_startup_probe" }

type cleanupStartupProbeWorker struct {
	river.WorkerDefaults[cleanupStartupProbeArgs]
}

func (*cleanupStartupProbeWorker) Work(context.Context, *river.Job[cleanupStartupProbeArgs]) error {
	return nil
}

func TestCleanupDailyRestartOnlyCatchesUpWhenDue(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	riverSchema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(Config{Namespace: "restart", Kinds: []Kind{{Name: "post", Uploads: []Upload{{Path: "file", Types: []string{"image/png"}, MaxBytes: 1024}}}}})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewPGJournal(pool, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := func() *river.Client[pgx.Tx] {
		t.Helper()
		jobs, err := NewJobs(JobsConfig{Store: idleCleanupStore{}, Registry: reg, Locker: PGLocker(pool), Journal: journal})
		if err != nil {
			t.Fatal(err)
		}
		workers := river.NewWorkers()
		river.AddWorker(workers, &cleanupStartupProbeWorker{})
		// A separate native probe makes this test wait for the new leader's
		// startup batch, without assuming how long its election takes.
		probe := river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour), func() (river.JobArgs, *river.InsertOpts) {
			return cleanupStartupProbeArgs{}, &river.InsertOpts{Queue: jobs.cfg.Queue}
		}, &river.PeriodicJobOpts{ID: "cleanup_startup_probe", RunOnStart: true})
		client, err := riverhelpers.New(t.Context(), pool, &river.Config{Schema: riverSchema,
			CompletedJobRetentionPeriod: 72 * time.Hour,
			Workers:                     workers, PeriodicJobs: []*river.PeriodicJob{probe},
			FetchPollInterval: 20 * time.Millisecond, FetchCooldown: 10 * time.Millisecond}, jobs.RiverJobs())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.StopAndCancel(context.Background()) })
		events, unsubscribe := client.Subscribe(river.EventKindJobCompleted)
		defer unsubscribe()
		if err := client.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case event := <-events:
				if event.Job.Kind == (cleanupStartupProbeArgs{}).Kind() {
					return client
				}
			case <-deadline.C:
				t.Fatal("new leader did not finish its startup batch")
			}
		}
	}
	awaitCompleted := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		var count int
		for time.Now().Before(deadline) {
			err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{riverSchema, "river_job"}.Sanitize()+" WHERE kind='contentkit_media_cleanup_due' AND state='completed'").Scan(&count)
			if err == nil && count >= want {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("daily cleanup completed %d startup runs, want %d", count, want)
	}
	first := start()
	awaitCompleted(1)
	if err := first.StopAndCancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := start()
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{riverSchema, "river_job"}.Sanitize()+" WHERE kind='contentkit_media_cleanup_due'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("recent daily cleanup ran again on restart: count=%d err=%v", count, err)
	}
	if err := second.StopAndCancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Keep the test fast while representing a genuinely overdue daily run.
	if _, err := pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{riverSchema, "river_job"}.Sanitize()+`
		SET created_at=now()-interval '25 hours', scheduled_at=now()-interval '25 hours',
		attempted_at=now()-interval '25 hours', finalized_at=now()-interval '25 hours'
		WHERE kind='contentkit_media_cleanup_due'`); err != nil {
		t.Fatal(err)
	}
	third := start()
	awaitCompleted(2)
	if err := third.StopAndCancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	producer, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema})
	if err != nil {
		t.Fatal(err)
	}
	one, err := producer.Insert(t.Context(), cleanupDueArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := producer.Insert(t.Context(), cleanupDueArgs{}, nil)
	if err != nil || one.Job.ID != two.Job.ID || !two.UniqueSkippedAsDuplicate {
		t.Fatalf("unfinished daily scans did not coalesce: first=%d second=%+v err=%v", one.Job.ID, two, err)
	}
}
