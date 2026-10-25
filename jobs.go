package contentkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/worker"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/signal"
)

// QueueContentKit is the queue RiverJobs contributes. Hosts may set its
// concurrency in the shared River config; it needs at least one worker.
const QueueContentKit = "contentkit"

// RiverJobs is ContentKit's contribution to the host's River fleet (its
// workers, periodic jobs and queues, media's included), for riverhelpers.New
// in Config.Database.RiverSchema; then pass the fleet to Start with
// WithRiverClient. Requesting it rules out ContentKit's own River.
func (e *Engine) RiverJobs() riverhelpers.Contribution {
	e.mu.Lock()
	e.requested = true
	e.mu.Unlock()
	return e.jobs.contribution()
}

// jobs is the engine's River composition: one per engine.
type jobs struct {
	e      *Engine
	mu     sync.Mutex
	client *river.Client[pgx.Tx]
}

func (j *jobs) bound() *river.Client[pgx.Tx] {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.client
}

// The engine's schedules; each periodic run is unique per its period.
const (
	searchSyncEvery     = 30 * time.Second // documents trail their writes by at most this
	preferenceSyncEvery = 30 * time.Second
	resyncEvery         = 24 * time.Hour
	repairEvery         = time.Hour
	reclassifyEvery     = 10 * time.Minute
	searchPruneEvery    = time.Hour
)

func (j *jobs) contribution() riverhelpers.Contribution {
	e := j.e
	own := riverhelpers.NewContribution("contentkit", func(_ context.Context, cfg *river.Config) error {
		if e.erasureRecovery {
			return ErrErasureReplayRequired
		}
		workers := e.cfg.Jobs.MaxWorkers
		if workers <= 0 {
			workers = 2
		}
		if q, ok := cfg.Queues[QueueContentKit]; ok && q.MaxWorkers < 1 {
			return fmt.Errorf("contentkit: queue %q needs workers", QueueContentKit)
		} else if !ok {
			cfg.Queues[QueueContentKit] = river.QueueConfig{MaxWorkers: workers}
		}
		for _, add := range []func() error{
			func() error { return river.AddWorkerSafely(cfg.Workers, &searchSyncWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &preferenceSyncWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &repairWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &reclassifyWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &searchPruneWorker{e: e}) },
		} {
			if err := add(); err != nil {
				return err
			}
		}
		// Startup checks the retained deadline; it does not rerun recent work.
		periodic := func(id string, every time.Duration, args river.JobArgs) {
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, riverhelpers.NewPeriodicJob(cfg, every,
				func() (river.JobArgs, *river.InsertOpts) {
					return args, &river.InsertOpts{Queue: QueueContentKit, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: every}}
				}, &river.PeriodicJobOpts{ID: id, RunOnStart: true}))
		}
		periodic("contentkit_search_sync", searchSyncEvery, searchSyncArgs{})
		periodic("contentkit_search_prune", searchPruneEvery, searchPruneArgs{})
		if e.classifier {
			periodic("contentkit_reclassify", reclassifyEvery, reclassifyArgs{})
		}
		if e.hub.SignalsConfigured() { // their runs skip while it is down
			periodic("contentkit_preference_sync", preferenceSyncEvery, preferenceSyncArgs{})
			periodic("contentkit_preference_resync", resyncEvery, preferenceSyncArgs{Resync: true})
			periodic("contentkit_projection_repair", repairEvery, repairArgs{})
		}
		return nil
	}, func(_ context.Context, b riverhelpers.Binding) error {
		if b.Pool != e.pool {
			return errors.New("contentkit: the River fleet must use Deps.Postgres")
		}
		j.mu.Lock()
		j.client = b.Client
		j.mu.Unlock()
		return nil
	}, func() error {
		j.mu.Lock()
		j.client = nil
		j.mu.Unlock()
		return nil
	})
	parts := []riverhelpers.Contribution{own, e.catalog.RiverJobs(), e.entities.RiverJobs()}
	if e.media != nil {
		parts = append(parts, e.media.versions.RiverJobs())
	}
	return riverhelpers.Group(parts...)
}

// rebuild queues search documents after an import. Counts and entity covers
// are read from committed content facets.
func (e *Engine) rebuild(ctx context.Context) error {
	c := e.jobs.bound()
	if c == nil {
		return nil
	}
	opts := func(queue string) *river.InsertOpts {
		return &river.InsertOpts{Queue: queue, MaxAttempts: 3, UniqueOpts: media.PendingOnce}
	}
	params := []river.InsertManyParams{{Args: searchSyncArgs{}, InsertOpts: opts(QueueContentKit)}}
	_, err := c.InsertMany(ctx, params)
	return err
}

// syncDocuments queues a search sync in the transaction that queued
// documents (q, or after it when q is none), at most one a second, so they
// follow writes at once; the periodic sync stays the backstop.
func (e *Engine) syncDocuments(ctx context.Context, q any) error {
	c := e.jobs.bound()
	if c == nil {
		return nil
	}
	opts := &river.InsertOpts{Queue: QueueContentKit, MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Second, ByState: media.PendingOnce.ByState}}
	var err error
	if tx, ok := q.(pgx.Tx); ok {
		_, err = c.InsertTx(ctx, tx, searchSyncArgs{}, opts)
	} else {
		_, err = c.Insert(ctx, searchSyncArgs{}, opts)
	}
	return err
}

type searchSyncArgs struct{}

func (searchSyncArgs) Kind() string { return "contentkit_search_sync" }

// searchSyncWorker maintains the keyword documents of content items and
// entities.
type searchSyncWorker struct {
	river.WorkerDefaults[searchSyncArgs]
	e *Engine
}

func (w *searchSyncWorker) Timeout(*river.Job[searchSyncArgs]) time.Duration { return 10 * time.Minute }

func (w *searchSyncWorker) Work(ctx context.Context, _ *river.Job[searchSyncArgs]) error {
	e := w.e
	return worker.SyncOnce(ctx, worker.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant,
		SupportedLanguages: e.cfg.Content.Languages, Sources: []worker.Source{e.catalog, e.entities}})
}

type searchPruneArgs struct{}

func (searchPruneArgs) Kind() string { return "contentkit_search_prune" }

// searchPruneWorker deletes recent searches hidden by a clear, of erased
// people, or past their retention.
type searchPruneWorker struct {
	river.WorkerDefaults[searchPruneArgs]
	e *Engine
}

func (w *searchPruneWorker) Work(ctx context.Context, _ *river.Job[searchPruneArgs]) error {
	_, err := w.e.discover.PruneSearches(ctx)
	return err
}

type preferenceSyncArgs struct {
	// Resync re-sends every exportable preference: the repair for sink loss.
	Resync bool `json:"resync,omitempty"`
}

func (preferenceSyncArgs) Kind() string { return "contentkit_preference_sync" }

// preferenceSyncWorker sends reactions and favorites into the signal plane.
type preferenceSyncWorker struct {
	river.WorkerDefaults[preferenceSyncArgs]
	e *Engine
}

func (w *preferenceSyncWorker) Work(ctx context.Context, job *river.Job[preferenceSyncArgs]) error {
	h := w.e.hub
	if !h.SignalPlane() {
		return nil
	}
	if job.Args.Resync {
		_, err := w.e.content.ResyncPreferences(ctx, h.SendPreferences)
		return err
	}
	_, err := w.e.content.SyncPreferences(ctx, h.SendPreferences)
	return err
}

type repairArgs struct{}

func (repairArgs) Kind() string { return "contentkit_projection_repair" }

// repairWorker repairs projections of keys ingested since the last runs:
// crash repair, overlapping two periods.
type repairWorker struct {
	river.WorkerDefaults[repairArgs]
	e *Engine
}

func (w *repairWorker) Timeout(*river.Job[repairArgs]) time.Duration { return 30 * time.Minute }

func (w *repairWorker) Work(ctx context.Context, job *river.Job[repairArgs]) error {
	opts := signal.RepairOptions{IngestedSince: job.CreatedAt.Add(-2 * repairEvery)}
	for {
		res, err := w.e.hub.RepairProjections(ctx, opts)
		if err != nil || res.Next == nil {
			return ignoreDisabled(err)
		}
		opts.After = res.Next
	}
}

type reclassifyArgs struct{}

func (reclassifyArgs) Kind() string { return "contentkit_reclassify" }

// reclassifyWorker retries free-text poll answers the classifier failed,
// sweeping every pending answer once per run.
type reclassifyWorker struct {
	river.WorkerDefaults[reclassifyArgs]
	e *Engine
}

func (w *reclassifyWorker) Timeout(*river.Job[reclassifyArgs]) time.Duration { return 30 * time.Minute }

func (w *reclassifyWorker) Work(ctx context.Context, _ *river.Job[reclassifyArgs]) error {
	var errs []error
	for after := ""; ; {
		page, err := w.e.content.ReclassifyPending(ctx, after, 100)
		if err != nil {
			errs = append(errs, err)
		}
		if page.Next == "" || page.Next == after || ctx.Err() != nil {
			return errors.Join(errs...)
		}
		after = page.Next
	}
}

func ignoreDisabled(err error) error {
	if errors.Is(err, hub.ErrSignalPlaneDisabled) {
		return nil
	}
	return err
}
