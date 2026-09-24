package video

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/media"
)

// Video jobs live in their own River schema in the host database, so the
// heavy worker never joins (or wins leadership of) the host's River client.
const (
	Schema      = "media_worker"
	Queue       = "media_video"
	MaxAttempts = 5
)

// Args is the River job: encode one manifest's video files.
type Args Job

func (Args) Kind() string { return "contentkit_media_video" }

// Migrate creates Schema and applies River's migrations. Hosts run it in
// their migrate step; the worker also runs it at start.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return riverhelpers.ApplyMigrations(ctx, pool, Schema)
}

// Enqueuer is the host's insert-only client for video jobs.
type Enqueuer struct {
	client *river.Client[pgx.Tx]
	kinds  *media.Registry
}

func NewEnqueuer(pool *pgxpool.Pool, kinds *media.Registry) (*Enqueuer, error) {
	if pool == nil || kinds == nil {
		return nil, errors.New("media/video: Enqueuer needs a pool and a Registry")
	}
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: Schema})
	if err != nil {
		return nil, err
	}
	return &Enqueuer{client: c, kinds: kinds}, nil
}

// Enqueue inserts a job for a video kind's manifest; slot jobs and other
// kinds are not video work and are ignored.
func (q *Enqueuer) Enqueue(ctx context.Context, job media.ProcessJob) error {
	args, ok, err := q.args(job)
	if !ok || err != nil {
		return err
	}
	_, err = q.client.Insert(ctx, args, insertOpts())
	return err
}

// Processor hands commits of video kinds to the worker: register it with
// media.Jobs.AddProcessor so the host's process job inserts into Schema.
func (q *Enqueuer) Processor() media.Processor { return q.Enqueue }

// EnqueueTx inserts the job in the host's transaction.
func (q *Enqueuer) EnqueueTx(ctx context.Context, tx pgx.Tx, job media.ProcessJob) error {
	args, ok, err := q.args(job)
	if !ok || err != nil {
		return err
	}
	_, err = q.client.InsertTx(ctx, tx, args, insertOpts())
	return err
}

func (q *Enqueuer) args(job media.ProcessJob) (Args, bool, error) {
	if job.Slot != "" {
		return Args{}, false, nil
	}
	item, err := q.kinds.Item(job.Ref)
	if err != nil {
		return Args{}, false, err
	}
	v := item.Kind().Video
	if v == nil {
		return Args{}, false, nil
	}
	if _, err := item.ManifestKey(); err != nil {
		return Args{}, false, err
	}
	return Args{Ref: job.Ref, Versioned: item.Kind().Versioned, Video: *v}, true, nil
}

// Jobs are not unique: River's uniqueness always covers running jobs, which
// would drop the job for a source replaced mid-encode. Duplicates serialize
// on a per-manifest lock and are no-ops once the manifest is fresh.
func insertOpts() *river.InsertOpts {
	return &river.InsertOpts{Queue: Queue, MaxAttempts: MaxAttempts}
}

// WorkerConfig configures the River side of cmd/media-worker.
type WorkerConfig struct {
	Encoder *Encoder
	Pool    *pgxpool.Pool
	// Timeout bounds one job. It must exceed the worst case: a 2 h 4K ladder on
	// 2 CPU runs for many hours. Default 48 h; River rescues a job stuck an
	// hour past it.
	Timeout    time.Duration
	MaxWorkers int // concurrent jobs per process; default 1 (ffmpeg uses every core)
	Logger     *slog.Logger
}

// Contribution registers the video worker and queue for riverhelpers.New on
// a client with Schema.
func Contribution(c WorkerConfig) (riverhelpers.Contribution, error) {
	if c.Encoder == nil || c.Pool == nil {
		return riverhelpers.Contribution{}, errors.New("media/video: WorkerConfig needs an Encoder and a Pool")
	}
	if c.Timeout <= 0 {
		c.Timeout = 48 * time.Hour
	}
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 1
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return riverhelpers.NewContribution("contentkit-media-video", func(_ context.Context, cfg *river.Config) error {
		if cfg.Schema != Schema {
			return fmt.Errorf("media/video: River schema must be %q, not %q", Schema, cfg.Schema)
		}
		if cfg.JobTimeout < c.Timeout {
			return fmt.Errorf("media/video: client JobTimeout %s is below the video timeout %s", cfg.JobTimeout, c.Timeout)
		}
		if _, ok := cfg.Queues[Queue]; ok {
			return fmt.Errorf("media/video: queue %q already registered", Queue)
		}
		cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: c.MaxWorkers}
		return river.AddWorkerSafely(cfg.Workers, &worker{c: c})
	}, nil, nil), nil
}

// ClientConfig is the worker's River client configuration for c.
func ClientConfig(c WorkerConfig) *river.Config {
	if c.Timeout <= 0 {
		c.Timeout = 48 * time.Hour
	}
	return &river.Config{Schema: Schema, JobTimeout: c.Timeout, Logger: c.Logger}
}

type worker struct {
	river.WorkerDefaults[Args]
	c WorkerConfig
}

func (w *worker) Timeout(*river.Job[Args]) time.Duration { return w.c.Timeout }

// Work runs one encode under a per-manifest lock; a duplicate job for a
// manifest being encoded waits by snoozing.
func (w *worker) Work(ctx context.Context, job *river.Job[Args]) error {
	conn, err := w.c.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lock := "contentkit:media:video:" + job.Args.Ref.String()
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", lock).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return river.JobSnooze(time.Minute)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lock); err != nil {
			_ = conn.Conn().Close(ctx)
		}
	}()
	err = w.c.Encoder.Encode(ctx, Job(job.Args))
	var perm *PermanentError
	if errors.As(err, &perm) {
		w.c.Logger.ErrorContext(ctx, "media/video: cannot encode", "ref", job.Args.Ref.String(), "error", err)
		return river.JobCancel(err)
	}
	return err
}
