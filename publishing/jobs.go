package publishing

import (
	"context"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media/versions"
)

type reconcileArgs struct {
	Tenant string `json:"tenant"`
}

func (reconcileArgs) Kind() string { return "contentkit_publishing_reconcile" }

// RiverJobs joins the existing version queue. The durable operation table is
// the work source; a periodic reconciliation also recovers interrupted jobs.
func (s *Store) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-publishing", func(_ context.Context, cfg *river.Config) error {
		if err := river.AddWorkerSafely(cfg.Workers, &reconcileWorker{s: s}); err != nil {
			return err
		}
		if _, ok := cfg.Queues[versions.Queue]; !ok {
			cfg.Queues[versions.Queue] = river.QueueConfig{MaxWorkers: 2}
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(5*time.Second), func() (river.JobArgs, *river.InsertOpts) {
			return reconcileArgs{Tenant: s.o.Tenant}, &river.InsertOpts{Queue: versions.Queue, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 5 * time.Second}}
		}, &river.PeriodicJobOpts{ID: "contentkit_publishing_reconcile:" + s.o.Tenant, RunOnStart: true}))
		return nil
	}, nil, nil)
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
