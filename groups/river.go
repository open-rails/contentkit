package groups

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// ReconcileInterval is how often the engine syncs every group whose scope or
// owned content does not match its state yet, and purges deleted groups'
// media past the retention.
const ReconcileInterval = time.Minute

type syncArgs struct {
	Tenant string   `json:"tenant"`
	Groups []string `json:"groups"`
}

func (syncArgs) Kind() string { return "contentkit_group_sync" }

type refreshArgs struct {
	Tenant string   `json:"tenant"`
	Groups []string `json:"groups"`
}

func (refreshArgs) Kind() string { return "contentkit_group_refresh" }

type refreshWorker struct {
	river.WorkerDefaults[refreshArgs]
	s *Store
}

func (w *refreshWorker) Work(ctx context.Context, job *river.Job[refreshArgs]) error {
	if job.Args.Tenant != w.s.tenant {
		return river.JobCancel(fmt.Errorf("groups: job of tenant %q", job.Args.Tenant))
	}
	return w.s.RefreshGroupMembers(ctx, job.Args.Groups)
}

type reconcileArgs struct {
	Tenant string `json:"tenant"`
}

func (reconcileArgs) Kind() string { return "contentkit_group_reconcile" }

type syncWorker struct {
	river.WorkerDefaults[syncArgs]
	s *Store
}

func (w *syncWorker) Work(ctx context.Context, job *river.Job[syncArgs]) error {
	if job.Args.Tenant != w.s.tenant {
		return river.JobCancel(fmt.Errorf("groups: job of tenant %q", job.Args.Tenant))
	}
	ids := make([]GroupID, len(job.Args.Groups))
	for i, id := range job.Args.Groups {
		ids[i] = GroupID(id)
	}
	return w.s.Sync(ctx, ids)
}

type reconcileWorker struct {
	river.WorkerDefaults[reconcileArgs]
	s *Store
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[reconcileArgs]) error {
	return w.s.Reconcile(ctx)
}

// riverBinding is the River client a store's composition bound.
type riverBinding struct {
	mu       sync.Mutex
	composed bool
	client   *river.Client[pgx.Tx]
}

func (b *riverBinding) bound() *river.Client[pgx.Tx] {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client
}

// RiverJobs contributes the group sync worker, its queue and the periodic
// reconcile to the engine's helpers/river composition. It composes once;
// until bound, writes leave their syncs to the reconcile.
func (s *Store) RiverJobs() riverhelpers.Contribution {
	b := s.river
	claimed := false
	return riverhelpers.NewContribution("contentkit-groups", func(_ context.Context, cfg *river.Config) error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.composed {
			return errors.New("groups: River jobs already composed")
		}
		b.composed, claimed = true, true
		if _, ok := cfg.Queues[s.queue]; !ok {
			cfg.Queues[s.queue] = river.QueueConfig{MaxWorkers: 1}
		}
		if err := river.AddWorkerSafely(cfg.Workers, &syncWorker{s: s}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &reconcileWorker{s: s}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &refreshWorker{s: s}); err != nil {
			return err
		}
		if s.scopes != nil || s.owned != nil || s.folders != nil {
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(ReconcileInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return reconcileArgs{Tenant: s.tenant}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 3,
						UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: ReconcileInterval}}
				}, &river.PeriodicJobOpts{ID: "contentkit_group_reconcile_" + s.tenant, RunOnStart: true}))
		}
		return nil
	}, func(_ context.Context, bd riverhelpers.Binding) error {
		b.mu.Lock()
		b.client = bd.Client
		b.mu.Unlock()
		return nil
	}, func() error {
		if claimed {
			b.mu.Lock()
			b.client = nil
			b.mu.Unlock()
		}
		return nil
	})
}
