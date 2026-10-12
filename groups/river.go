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

// ScopeReconcileInterval is how often the engine syncs every group whose
// AuthKit scope does not match it yet.
const ScopeReconcileInterval = time.Minute

type scopeArgs struct {
	Tenant string   `json:"tenant"`
	Groups []string `json:"groups"`
}

func (scopeArgs) Kind() string { return "contentkit_group_scopes" }

type scopeReconcileArgs struct {
	Tenant string `json:"tenant"`
}

func (scopeReconcileArgs) Kind() string { return "contentkit_group_scopes_reconcile" }

type scopeWorker struct {
	river.WorkerDefaults[scopeArgs]
	s *Store
}

func (w *scopeWorker) Work(ctx context.Context, job *river.Job[scopeArgs]) error {
	if job.Args.Tenant != w.s.tenant {
		return river.JobCancel(fmt.Errorf("groups: job of tenant %q", job.Args.Tenant))
	}
	ids := make([]GroupID, len(job.Args.Groups))
	for i, id := range job.Args.Groups {
		ids[i] = GroupID(id)
	}
	return w.s.SyncScopes(ctx, ids)
}

type scopeReconcileWorker struct {
	river.WorkerDefaults[scopeReconcileArgs]
	s *Store
}

func (w *scopeReconcileWorker) Work(ctx context.Context, _ *river.Job[scopeReconcileArgs]) error {
	return w.s.ReconcileScopes(ctx)
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

// RiverJobs contributes the scope sync worker, its queue and the periodic
// reconcile to the engine's helpers/river composition. It composes once;
// until bound, writes leave their scopes to the reconcile.
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
		if err := river.AddWorkerSafely(cfg.Workers, &scopeWorker{s: s}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &scopeReconcileWorker{s: s}); err != nil {
			return err
		}
		if s.scopes != nil {
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(ScopeReconcileInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return scopeReconcileArgs{Tenant: s.tenant}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 3,
						UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: ScopeReconcileInterval}}
				}, &river.PeriodicJobOpts{ID: "contentkit_group_scopes_reconcile_" + s.tenant, RunOnStart: true}))
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
