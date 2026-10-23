package offers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	riverhelpers "github.com/open-rails/helpers/river"
)

// ReconcileInterval is how often the engine queues a sync of every offer
// OpenRails does not match yet.
const ReconcileInterval = time.Minute

type syncArgs struct {
	Tenant   string `json:"tenant"`
	Target   Target `json:"target"`
	Revision int64  `json:"revision,omitempty"`
}

func (syncArgs) Kind() string { return "contentkit_offer_sync" }

type reconcileArgs struct {
	Tenant string `json:"tenant"`
}

func (reconcileArgs) Kind() string { return "contentkit_offer_reconcile" }

type syncWorker struct {
	river.WorkerDefaults[syncArgs]
	s *Service
}

func (w *syncWorker) Work(ctx context.Context, job *river.Job[syncArgs]) error {
	if job.Args.Tenant != w.s.tenant {
		return river.JobCancel(fmt.Errorf("offers: job of tenant %q", job.Args.Tenant))
	}
	return w.s.Sync(ctx, []Target{job.Args.Target})
}

type reconcileWorker struct {
	river.WorkerDefaults[reconcileArgs]
	s *Service
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[reconcileArgs]) error {
	return w.s.Reconcile(ctx)
}

// Reconcile queues a sync of every target with an offer OpenRails does not
// match yet: those whose job was lost, whose sync keeps failing, or whose
// active offer is no longer permitted by the host's current kind policy.
func (s *Service) Reconcile(ctx context.Context) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT o.target_kind, o.target_id FROM `+s.t.offers+` o
 LEFT JOIN `+s.t.versions+` v ON o.target_kind = 'version' AND v.tenant_id = o.tenant_id AND v.id = o.target_id
 WHERE o.tenant_id = $1 AND (o.synced_revision < o.revision OR (o.state = 'active' AND o.target_kind <> 'group'
 AND NOT EXISTS (SELECT 1 FROM `+s.t.items+` c WHERE c.tenant_id=o.tenant_id
  AND c.id = CASE WHEN o.target_kind = 'content' THEN o.target_id ELSE v.content_id END AND c.kind = ANY($2::text[])
  AND c.state <> 'deleted' AND (o.target_kind <> 'version' OR v.state <> 'deleted'))))`, s.tenant, s.sellableKinds())
		if err != nil {
			return err
		}
		targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Target, error) {
			var t Target
			return t, r.Scan(&t.Kind, &t.ID)
		})
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, targets)
	})
}

// syncOpts keeps one waiting or running sync per target revision. A running
// sync may already have read an older revision, so a follow-up edit uses new
// args rather than being suppressed. Sync serializes provider writes for a
// target under its advisory lock. An edit names a version's content, the
// reconcile does not, so a version can briefly have two jobs for one revision.
func (s *Service) syncOpts() *river.InsertOpts {
	return &river.InsertOpts{Queue: s.queue, MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
		rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// RiverJobs contributes the offer sync worker, its queue and the periodic
// reconcile to the engine's helpers/river composition. It composes once;
// until bound, Put and Withdraw leave syncing to the reconcile.
func (s *Service) RiverJobs() riverhelpers.Contribution {
	claimed := false
	return riverhelpers.NewContribution("contentkit-offers", func(_ context.Context, cfg *river.Config) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.composed {
			return errors.New("offers: River jobs already composed")
		}
		s.composed, claimed = true, true
		if _, ok := cfg.Queues[s.queue]; !ok {
			cfg.Queues[s.queue] = river.QueueConfig{MaxWorkers: 2}
		}
		if err := river.AddWorkerSafely(cfg.Workers, &syncWorker{s: s}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &reconcileWorker{s: s}); err != nil {
			return err
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(ReconcileInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return reconcileArgs{Tenant: s.tenant}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 3,
					UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: ReconcileInterval}}
			}, &river.PeriodicJobOpts{ID: "contentkit_offer_reconcile_" + s.tenant, RunOnStart: true}))
		return nil
	}, func(_ context.Context, b riverhelpers.Binding) error {
		s.mu.Lock()
		s.client = b.Client
		s.mu.Unlock()
		return nil
	}, func() error {
		if claimed {
			s.mu.Lock()
			s.client = nil
			s.mu.Unlock()
		}
		return nil
	})
}

func (s *Service) bound() *river.Client[pgx.Tx] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}
