package versions

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media"
)

// Queue is the River queue of versions' media jobs.
const Queue = "contentkit_versions"

// poll is how soon a job waiting on the media worker runs again.
const poll = 5 * time.Second

// publishArgs brings a version's publications to its media.
type publishArgs struct {
	Content string `json:"content"`
	Version string `json:"version"`
	After   int64  `json:"after,omitempty"` // the running job this one follows
}

func (publishArgs) Kind() string                      { return "contentkit_version_publish" }
func (a publishArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

// RiverJobs contributes the version release workers and queue to
// the host's River composition, beside media's own.
func (s *Service) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-versions", func(_ context.Context, cfg *river.Config) error {
		for _, add := range []func(*river.Workers) error{
			func(w *river.Workers) error { return river.AddWorkerSafely(w, &publishWorker{s: s}) },
			s.addReleaseWorker,
		} {
			if err := add(cfg.Workers); err != nil {
				return err
			}
		}
		if _, ok := cfg.Queues[QueueFor(s.o.Tenant)]; !ok {
			cfg.Queues[QueueFor(s.o.Tenant)] = river.QueueConfig{MaxWorkers: 2}
		}
		return nil
	}, func(_ context.Context, b riverhelpers.Binding) error {
		if b.Pool != s.o.Pool {
			return errors.New("versions: River must use the service's pool")
		}
		s.mu.Lock()
		s.client = b.Client
		s.mu.Unlock()
		return nil
	}, func() error {
		s.mu.Lock()
		s.client = nil
		s.mu.Unlock()
		return nil
	})
}

func (s *Service) bound() (*river.Client[pgx.Tx], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil, errors.New("versions: River jobs are not composed into a client")
	}
	return s.client, nil
}

// enqueueTx queues a job in tx; an equal one waiting absorbs it, and one
// running gets a follow-up.
func (s *Service) enqueueTx(ctx context.Context, tx pgx.Tx, args media.RerunArgs) error {
	c, err := s.bound()
	if err != nil {
		return err
	}
	return media.InsertOnce(ctx, func(ctx context.Context, a river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
		return c.InsertTx(ctx, tx, a, o)
	}, args, river.InsertOpts{Queue: QueueFor(s.o.Tenant)})
}

// outcome maps a job's error to River's: gone cancels, waiting snoozes.
func outcome(err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, errFailed):
		return river.JobCancel(err)
	case errors.Is(err, ErrNotReady), errors.Is(err, errRetired):
		return river.JobSnooze(poll)
	}
	return err
}

type publishWorker struct {
	river.WorkerDefaults[publishArgs]
	s *Service
}

func (w *publishWorker) Timeout(*river.Job[publishArgs]) time.Duration { return 30 * time.Minute }

func (w *publishWorker) Work(ctx context.Context, job *river.Job[publishArgs]) error {
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	err := w.s.publish(ctx, Target{Content: job.Args.Content, Version: job.Args.Version})
	if errors.Is(err, ErrNotReady) {
		// Commits and durable processor completions enqueue the next
		// snapshot attempt, including a follow-up if this job is still running.
		return nil
	}
	return outcome(err)
}

// publish refreshes prepared metadata and schedules durable placement. Preparing
// media alone never replaces the completed reader snapshot.
func (s *Service) publish(ctx context.Context, t Target) error {
	v, err := s.one(ctx, t)
	if err != nil {
		return err
	}
	k, err := s.reg.Kind(v.Kind)
	if err != nil {
		return err
	}
	if err := s.refreshMediaFacts(ctx, v.Kind, t); err != nil {
		return err
	}
	if k.Publish != nil {
		if err := s.preparePageSelection(ctx, k, t); err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
		if err := s.applyCover(ctx, k, t); err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		return s.SchedulePublicationTx(ctx, tx, []Target{t})
	})
}
