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

// importArgs expands a version's page archives.
type importArgs struct {
	Content string `json:"content"`
	Version string `json:"version"`
	After   int64  `json:"after,omitempty"`
}

func (importArgs) Kind() string                      { return "contentkit_version_import" }
func (a importArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

// forkArgs fills a version from a sibling (Fork).
type forkArgs struct {
	Content  string `json:"content"`
	Version  string `json:"version"`
	From     string `json:"from"`
	Playback bool   `json:"playback,omitempty"`
	Segments bool   `json:"segments,omitempty"`
}

func (forkArgs) Kind() string { return "contentkit_version_fork" }

// recoverArgs requeues builds left behind and previews a release changed.
type recoverArgs struct{}

func (recoverArgs) Kind() string { return "contentkit_version_publish_recovery" }

// RiverJobs contributes the versions' workers, queue and recovery pass to
// the host's River composition, beside media's own.
func (s *Service) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-versions", func(_ context.Context, cfg *river.Config) error {
		for _, add := range []func(*river.Workers) error{
			func(w *river.Workers) error { return river.AddWorkerSafely(w, &publishWorker{s: s}) },
			func(w *river.Workers) error { return river.AddWorkerSafely(w, &importWorker{s: s}) },
			func(w *river.Workers) error { return river.AddWorkerSafely(w, &forkWorker{s: s}) },
			func(w *river.Workers) error { return river.AddWorkerSafely(w, &recoverWorker{s: s}) },
		} {
			if err := add(cfg.Workers); err != nil {
				return err
			}
		}
		if _, ok := cfg.Queues[Queue]; !ok {
			cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: 2}
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
			func() (river.JobArgs, *river.InsertOpts) {
				return recoverArgs{}, &river.InsertOpts{Queue: Queue, UniqueOpts: media.PendingOnce}
			}, &river.PeriodicJobOpts{ID: "contentkit_version_publish_recovery"}))
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
	}, args, river.InsertOpts{Queue: Queue})
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
	return outcome(w.s.publish(ctx, Target{Content: job.Args.Content, Version: job.Args.Version}))
}

// publish brings v's pages publication, previews and playback to its media.
func (s *Service) publish(ctx context.Context, t Target) error {
	v, err := s.one(ctx, t)
	if err != nil {
		return err
	}
	k, err := s.published(v)
	if err != nil {
		return err
	}
	ready, err := s.publishPages(ctx, k, t)
	if err != nil {
		return err
	}
	if !ready {
		return ErrNotReady
	}
	if err := s.applyPreviews(ctx, k, t); err != nil {
		return err
	}
	if k.Publish.Playback == "" {
		return nil
	}
	if ready, err = s.publishPlayback(ctx, k, t); err != nil {
		return err
	}
	if !ready {
		return ErrNotReady
	}
	return nil
}

type importWorker struct {
	river.WorkerDefaults[importArgs]
	s *Service
}

func (w *importWorker) Timeout(*river.Job[importArgs]) time.Duration { return time.Hour }

func (w *importWorker) Work(ctx context.Context, job *river.Job[importArgs]) error {
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	return outcome(w.s.expand(ctx, Target{Content: job.Args.Content, Version: job.Args.Version}))
}

type forkWorker struct {
	river.WorkerDefaults[forkArgs]
	s *Service
}

func (w *forkWorker) Timeout(*river.Job[forkArgs]) time.Duration { return time.Hour }

func (w *forkWorker) Work(ctx context.Context, job *river.Job[forkArgs]) error {
	err := w.s.fork(ctx, job.Args)
	if errors.Is(err, errForkConflict) {
		return river.JobCancel(err)
	}
	return outcome(err)
}

type recoverWorker struct {
	river.WorkerDefaults[recoverArgs]
	s *Service
}

func (w *recoverWorker) Work(ctx context.Context, _ *river.Job[recoverArgs]) error {
	return w.s.recoverPublications(ctx)
}

// recoverPublications requeues builds a minute old and current pages whose
// previews a segment's release since outdated.
func (s *Service) recoverPublications(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT v.content_id, v.id FROM `+s.t.publications+` p
JOIN `+s.t.versions+` v ON v.tenant_id = p.tenant_id AND v.id = p.version_id
WHERE p.tenant_id = $1 AND (p.state = 'building' AND p.created_at < now() - interval '1 minute'
	OR p.kind = 'pages' AND p.state = 'current' AND (p.previews_at IS NULL OR EXISTS (SELECT 1 FROM `+s.t.segments+` g
		WHERE g.tenant_id = p.tenant_id AND g.version_id = p.version_id AND g.release_at <= now() AND g.release_at > p.previews_at)))
LIMIT 500`, s.o.Tenant)
		if err != nil {
			return err
		}
		due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Target])
		if err != nil {
			return err
		}
		for _, t := range due {
			if err := s.enqueueTx(ctx, tx, publishArgs{Content: t.Content, Version: t.Version}); err != nil {
				return err
			}
		}
		return nil
	})
}
