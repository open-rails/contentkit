package publishing

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

type reconcileArgs struct {
	Tenant string `json:"tenant"`
}

func (reconcileArgs) Kind() string { return "contentkit_publishing_reconcile" }

type operationArgs struct {
	Tenant string `json:"tenant"`
	ID     string `json:"id"`
}

func (operationArgs) Kind() string { return "contentkit_publishing_operation" }

const reconcileInterval = time.Minute

// RiverJobs joins the existing version queue. Completed uploads enqueue a
// durable monitor; only active operations poll media readiness. The periodic
// pass recovers missing monitors and notices review/authority changes.
func (s *Store) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-publishing", func(_ context.Context, cfg *river.Config) error {
		if err := river.AddWorkerSafely(cfg.Workers, &reconcileWorker{s: s}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &operationWorker{s: s}); err != nil {
			return err
		}
		if _, ok := cfg.Queues[versions.Queue]; !ok {
			cfg.Queues[versions.Queue] = river.QueueConfig{MaxWorkers: 2}
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, riverhelpers.NewPeriodicJob(cfg, reconcileInterval, func() (river.JobArgs, *river.InsertOpts) {
			return reconcileArgs{Tenant: s.o.Tenant}, &river.InsertOpts{Queue: versions.Queue, UniqueOpts: media.PendingOnce}
		}, &river.PeriodicJobOpts{ID: "contentkit_publishing_reconcile:" + s.o.Tenant, RunOnStart: true}))
		return nil
	}, func(_ context.Context, binding riverhelpers.Binding) error {
		s.mu.Lock()
		s.jobs = binding.Client
		s.mu.Unlock()
		return nil
	}, func() error {
		s.mu.Lock()
		s.jobs = nil
		s.mu.Unlock()
		return nil
	})
}

func (s *Store) client() *river.Client[pgx.Tx] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs
}

func (s *Store) enqueueOperation(ctx context.Context, tx pgx.Tx, id string) error {
	client := s.client()
	if client == nil {
		return nil
	}
	args := operationArgs{Tenant: s.o.Tenant, ID: id}
	opts := &river.InsertOpts{Queue: versions.Queue, UniqueOpts: media.PendingOnce}
	_, err := client.InsertTx(ctx, tx, args, opts)
	return err
}

func (s *Store) recoverOperation(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		// Rotate even when an already queued/retrying monitor absorbs this
		// wake-up, so the first batch cannot hide later missing monitors.
		// Lock the operation before its job, matching Complete's lock order.
		if _, err := tx.Exec(ctx, `UPDATE `+s.table+` SET reconcile_at=now() WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, id); err != nil {
			return err
		}
		return s.enqueueOperation(ctx, tx, id)
	})
}

type operationWorker struct {
	river.WorkerDefaults[operationArgs]
	s *Store
}

func (w *operationWorker) Timeout(*river.Job[operationArgs]) time.Duration { return 2 * time.Minute }

func (w *operationWorker) Work(ctx context.Context, job *river.Job[operationArgs]) error {
	if job.Args.Tenant != w.s.o.Tenant {
		return nil
	}
	if err := w.s.reconcile(ctx, job.Args.ID); err != nil {
		return err
	}
	r, err := w.s.load(ctx, w.s.o.Pool, job.Args.ID)
	if err != nil {
		return err
	}
	if delay := operationDelay(r, w.s.o.Clock()); delay > 0 {
		return river.JobSnooze(delay)
	}
	return nil
}

func operationDelay(r record, now time.Time) time.Duration {
	switch r.State {
	case Processing, Publishing:
		return 5 * time.Second
	case Scheduled:
		if r.request.PublishAt != nil && r.request.PublishAt.After(now) {
			return r.request.PublishAt.Sub(now)
		}
		return 5 * time.Second // the catalog's due-publication transaction may still be running
	case Blocked:
		if r.Error != nil && r.Error.Retryable {
			return reconcileInterval
		}
	}
	return 0
}

type reconcileWorker struct {
	river.WorkerDefaults[reconcileArgs]
	s *Store
}

func (w *reconcileWorker) Timeout(*river.Job[reconcileArgs]) time.Duration { return 2 * time.Minute }
func (w *reconcileWorker) Work(ctx context.Context, job *river.Job[reconcileArgs]) error {
	if job.Args.Tenant != w.s.o.Tenant {
		return nil
	}
	return w.s.Reconcile(ctx)
}
