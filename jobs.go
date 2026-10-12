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

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/worker"
	"github.com/open-rails/contentkit/search"
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
	coEngagementEvery   = 24 * time.Hour
	repairEvery         = time.Hour
	erasureEvery        = 24 * time.Hour
	reclassifyEvery     = 10 * time.Minute
)

func (j *jobs) contribution() riverhelpers.Contribution {
	e := j.e
	own := riverhelpers.NewContribution("contentkit", func(_ context.Context, cfg *river.Config) error {
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
			func() error { return river.AddWorkerSafely(cfg.Workers, &coEngagementWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &repairWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &erasureWorker{e: e}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &reclassifyWorker{e: e}) },
		} {
			if err := add(); err != nil {
				return err
			}
		}
		periodic := func(id string, every time.Duration, args river.JobArgs, onStart bool) {
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(every),
				func() (river.JobArgs, *river.InsertOpts) {
					return args, &river.InsertOpts{Queue: QueueContentKit, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: every}}
				}, &river.PeriodicJobOpts{ID: id, RunOnStart: onStart}))
		}
		periodic("contentkit_search_sync", searchSyncEvery, searchSyncArgs{}, true)
		if e.classifier {
			periodic("contentkit_reclassify", reclassifyEvery, reclassifyArgs{}, false)
		}
		if e.hub.SignalPlane() {
			periodic("contentkit_preference_sync", preferenceSyncEvery, preferenceSyncArgs{}, true)
			periodic("contentkit_preference_resync", resyncEvery, preferenceSyncArgs{Resync: true}, false)
			periodic("contentkit_coengagement", coEngagementEvery, coEngagementArgs{}, false)
			periodic("contentkit_projection_repair", repairEvery, repairArgs{}, true)
			periodic("contentkit_erasure", erasureEvery, erasureArgs{}, false)
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
	if e.media == nil {
		return own
	}
	return riverhelpers.Group(own, e.media.jobs.RiverJobs())
}

type searchSyncArgs struct{}

func (searchSyncArgs) Kind() string { return "contentkit_search_sync" }

// searchSyncWorker maintains the keyword documents of the kinds ContentKit
// owns: posts and taxonomy nodes.
type searchSyncWorker struct {
	river.WorkerDefaults[searchSyncArgs]
	e *Engine
}

func (w *searchSyncWorker) Timeout(*river.Job[searchSyncArgs]) time.Duration { return 10 * time.Minute }

func (w *searchSyncWorker) Work(ctx context.Context, _ *river.Job[searchSyncArgs]) error {
	e := w.e
	list := worker.ListContentPage(func(ctx context.Context, tenant, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
		if kind == content.KindPost {
			return e.content.ListContent(ctx, tenant, kind, language, cursor, limit)
		}
		return nil, "", false, fmt.Errorf("contentkit: no documents for kind %q", kind)
	})
	build := worker.BuildKeywordDocuments(func(ctx context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		if kind == content.KindPost {
			return e.content.KeywordDocuments(ctx, tenant, kind, language, refs)
		}
		return nil, fmt.Errorf("contentkit: no documents for kind %q", kind)
	})
	kinds := []string{content.KindPost}
	if e.taxonomy != nil {
		list, build = e.taxonomy.Lister(list), e.taxonomy.Builder(build)
		for k := range e.cfg.Content.GroupKinds {
			kinds = append(kinds, k)
		}
	}
	return worker.SyncOnce(ctx, worker.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant,
		SupportedLanguages: e.cfg.Content.Languages, ContentKinds: kinds, ListContent: list, BuildKeywordDocuments: build})
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

type coEngagementArgs struct{}

func (coEngagementArgs) Kind() string { return "contentkit_coengagement" }

// coEngagementWorker rematerializes the co-engagement rollup.
type coEngagementWorker struct {
	river.WorkerDefaults[coEngagementArgs]
	e *Engine
}

func (w *coEngagementWorker) Timeout(*river.Job[coEngagementArgs]) time.Duration { return time.Hour }

func (w *coEngagementWorker) Work(ctx context.Context, _ *river.Job[coEngagementArgs]) error {
	return ignoreDisabled(w.e.hub.RefreshCoEngagement(ctx, signal.RefreshCoEngagementOptions{}))
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

type erasureArgs struct{}

func (erasureArgs) Kind() string { return "contentkit_erasure" }

// erasureWorker removes residue of every recorded erasure, also after a
// restore.
type erasureWorker struct {
	river.WorkerDefaults[erasureArgs]
	e *Engine
}

func (w *erasureWorker) Timeout(*river.Job[erasureArgs]) time.Duration { return time.Hour }

func (w *erasureWorker) Work(ctx context.Context, _ *river.Job[erasureArgs]) error {
	report, err := w.e.hub.EnforceErasures(ctx)
	if err = ignoreDisabled(err); err != nil {
		return err
	}
	if !report.Complete() {
		return fmt.Errorf("contentkit: erasure incomplete: %v", report.Remaining)
	}
	return nil
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
