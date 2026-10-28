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
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/internal/jobhook"
	"github.com/open-rails/contentkit/internal/worker"
	"github.com/open-rails/contentkit/media"
)

// queueContentKit is the queue of the engine's own jobs.
const queueContentKit = "contentkit"

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
	preferenceSyncEvery = 5 * time.Minute
	resyncEvery         = 24 * time.Hour
	searchPruneEvery    = time.Hour
	popularRefreshEvery = 5 * time.Minute
)

// contribution is the engine's River composition: its own workers, queues and
// schedules, the catalog's and the publication service's.
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
		if q, ok := cfg.Queues[queueContentKit]; ok && q.MaxWorkers < 1 {
			return fmt.Errorf("contentkit: queue %q needs workers", queueContentKit)
		} else if !ok {
			cfg.Queues[queueContentKit] = river.QueueConfig{MaxWorkers: workers}
		}
		for _, add := range []func() error{
			func() error { return river.AddWorkerSafely(cfg.Workers, &searchSyncWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &preferenceSyncWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &searchPruneWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &popularRefreshWorker{e: e}) },
		} {
			if err := add(); err != nil {
				return err
			}
		}
		// Startup checks the retained deadline; it does not rerun recent work.
		periodic := func(id string, every time.Duration, args river.JobArgs) {
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, riverhelpers.NewPeriodicJob(cfg, every,
				func() (river.JobArgs, *river.InsertOpts) {
					return args, &river.InsertOpts{Queue: queueContentKit, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: every}}
				}, &river.PeriodicJobOpts{ID: id, RunOnStart: true}))
		}
		periodic("contentkit_search_sync", searchSyncEvery, searchSyncArgs{})
		periodic("contentkit_search_prune", searchPruneEvery, searchPruneArgs{})
		if e.hub.SignalsConfigured() { // their runs skip while it is down
			periodic("contentkit_preference_sync", preferenceSyncEvery, preferenceSyncArgs{})
			periodic("contentkit_preference_resync", resyncEvery, preferenceSyncArgs{Resync: true})
			// A refresh can outlast its period; it never runs twice at once.
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, riverhelpers.NewPeriodicJob(cfg, popularRefreshEvery,
				func() (river.JobArgs, *river.InsertOpts) {
					return popularRefreshArgs{}, &river.InsertOpts{Queue: queueContentKit, MaxAttempts: 3, UniqueOpts: media.PendingOnce}
				}, &river.PeriodicJobOpts{ID: "contentkit_popular_refresh", RunOnStart: true}))
		}
		return nil
	}, func(_ context.Context, b riverhelpers.Binding) error {
		if b.Pool != e.pool {
			return errors.New("contentkit: River must use Deps.Postgres")
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
	parts := []riverhelpers.Contribution{own, e.catalog.RiverJobs(), e.versions.RiverJobs()}
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
	params := []river.InsertManyParams{{Args: searchSyncArgs{}, InsertOpts: opts(queueContentKit)}}
	_, err := c.InsertMany(ctx, params)
	return err
}

// syncDocuments queues a search sync in the transaction that queued
// documents (q, or after it when q is none), so they follow writes at once;
// the periodic sync stays the backstop. A sync already running may have read
// the queue before this write, so it gets one follow-up.
func (e *Engine) syncDocuments(ctx context.Context, q any) error {
	c := e.jobs.bound()
	if c == nil {
		return nil
	}
	insert := c.Insert
	if tx, ok := q.(pgx.Tx); ok {
		insert = func(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
			return c.InsertTx(ctx, tx, args, o)
		}
	}
	return media.InsertOnce(ctx, insert, searchSyncArgs{}, river.InsertOpts{Queue: queueContentKit, MaxAttempts: 3})
}

type searchSyncArgs struct {
	After int64 `json:"after,omitempty"`
}

func (searchSyncArgs) Kind() string                      { return "contentkit_search_sync" }
func (a searchSyncArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

// searchSyncWorker maintains the keyword documents of content items and
// entities.
type searchSyncWorker struct {
	river.WorkerDefaults[searchSyncArgs]
	e *Engine
}

func (w *searchSyncWorker) Timeout(*river.Job[searchSyncArgs]) time.Duration { return 10 * time.Minute }

func (w *searchSyncWorker) Work(ctx context.Context, job *river.Job[searchSyncArgs]) error {
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
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

type popularRefreshArgs struct{}

func (popularRefreshArgs) Kind() string { return "contentkit_popular_refresh" }

// popularRefreshWorker prepares reusable daily participation and refreshes
// host-declared popular rankings independently of public requests.
type popularRefreshWorker struct {
	river.WorkerDefaults[popularRefreshArgs]
	e *Engine
}

// Each ranking is stored as it completes, so a run cut short resumes with
// the rest on the next.
func (w *popularRefreshWorker) Timeout(*river.Job[popularRefreshArgs]) time.Duration {
	return time.Hour
}

func (w *popularRefreshWorker) Work(ctx context.Context, _ *river.Job[popularRefreshArgs]) error {
	return w.e.discover.RefreshPopularRankings(ctx)
}

func init() {
	jobhook.Fleet = func(ctx context.Context, engine any, cfg *river.Config, extra ...riverhelpers.Contribution) (*river.Client[pgx.Tx], error) {
		e := engine.(*Engine)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.started || e.closed || e.erasureRecovery {
			return nil, errors.New("contentkit: the engine cannot start")
		}
		fleet, err := riverhelpers.New(ctx, e.pool, cfg, append([]riverhelpers.Contribution{e.jobs.contribution()}, extra...)...)
		if err != nil {
			return nil, err
		}
		e.river, e.started = fleet, true
		return fleet, nil
	}
}
