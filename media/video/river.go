package video

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// WorkerConfig configures the River side of the media worker's video jobs
// (workqueue.VideoArgs on workqueue.VideoQueue).
type WorkerConfig struct {
	Encoder *Encoder
	Pool    *pgxpool.Pool
	Schema  string // worker River schema; default workqueue.Schema
	// Kinds is the host's registry: a job names only its ref, and the encode
	// takes the kind's ladder and bounds from here.
	Kinds *media.Registry
	// Timeout bounds one job. It must exceed the worst case: a 2 h 4K ladder on
	// 2 CPU runs for many hours. Default 48 h; River rescues a job stuck an
	// hour past it.
	Timeout    time.Duration
	MaxWorkers int // concurrent jobs per process; default 1 (ffmpeg uses every core)
	Logger     *slog.Logger
}

// Contribution registers the video worker and queue for riverhelpers.New on
// a client using c.Schema.
func Contribution(c WorkerConfig) (riverhelpers.Contribution, error) {
	if c.Encoder == nil || c.Pool == nil || c.Kinds == nil {
		return riverhelpers.Contribution{}, errors.New("media/video: WorkerConfig needs an Encoder, a Pool and Kinds")
	}
	c = c.defaults()
	return riverhelpers.NewContribution("contentkit-media-video", func(_ context.Context, cfg *river.Config) error {
		if cfg.Schema != c.Schema {
			return fmt.Errorf("media/video: River schema must be %q, not %q", c.Schema, cfg.Schema)
		}
		if cfg.JobTimeout < c.Timeout {
			return fmt.Errorf("media/video: client JobTimeout %s is below the video timeout %s", cfg.JobTimeout, c.Timeout)
		}
		if _, ok := cfg.Queues[workqueue.VideoQueue]; ok {
			return fmt.Errorf("media/video: queue %q already registered", workqueue.VideoQueue)
		}
		cfg.Queues[workqueue.VideoQueue] = river.QueueConfig{MaxWorkers: c.MaxWorkers}
		return river.AddWorkerSafely(cfg.Workers, &worker{c: c})
	}, nil, nil), nil
}

func (c WorkerConfig) defaults() WorkerConfig {
	c.Schema = cmp.Or(c.Schema, workqueue.Schema)
	if c.Timeout <= 0 {
		c.Timeout = 48 * time.Hour
	}
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 1
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// ClientConfig is a River client configuration running only c's video jobs.
func ClientConfig(c WorkerConfig) *river.Config {
	c = c.defaults()
	return &river.Config{Schema: c.Schema, JobTimeout: c.Timeout, Logger: c.Logger}
}

type worker struct {
	river.WorkerDefaults[workqueue.VideoArgs]
	c WorkerConfig
}

func (w *worker) Timeout(*river.Job[workqueue.VideoArgs]) time.Duration { return w.c.Timeout }

// Work runs one encode stage under a per-manifest lock; a duplicate job for
// a manifest being encoded waits by snoozing. A file left with a second
// stage gets a follow-up job at a lower priority, inserted after the lock is
// released, so other uploads' first stages run before it.
func (w *worker) Work(ctx context.Context, job *river.Job[workqueue.VideoArgs]) (err error) {
	item, err := w.c.Kinds.Item(job.Args.Ref)
	if err == nil && item.Kind().Video == nil {
		err = fmt.Errorf("media/video: kind %q has no video", item.Kind().Name)
	}
	if err != nil {
		return river.JobCancel(err)
	}
	var more bool
	defer func() {
		if err == nil && more {
			o := workqueue.VideoInsertOpts()
			o.Priority = 2
			_, err = river.ClientFromContext[pgx.Tx](ctx).Insert(ctx, job.Args, o)
		}
	}()
	// The lock's own connection lives outside Pool, which the encode uses.
	release, ok, err := pglock.Acquire(ctx, w.c.Pool, "contentkit:media:video:"+job.Args.Ref.String(), false)
	if err != nil {
		return err
	}
	if !ok {
		return river.JobSnooze(time.Minute)
	}
	defer release()
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := workqueue.ClearProgress(ctx, w.c.Pool, w.c.Schema, job.ID); err != nil {
			w.c.Logger.WarnContext(ctx, "media/video: clear progress", "job", job.ID, "error", err)
		}
	}()
	k := item.Kind()
	more, err = w.c.Encoder.encode(ctx, Job{Ref: job.Args.Ref, Versioned: k.Versioned, Video: *k.Video}, w.report(job.ID), true)
	var perm *PermanentError
	if errors.As(err, &perm) {
		w.c.Logger.ErrorContext(ctx, "media/video: cannot encode", "ref", job.Args.Ref.String(), "error", err)
		return river.JobCancel(err)
	}
	return err
}

func (w *worker) report(id int64) Report {
	return func(ctx context.Context, files map[string]media.EncodeProgress) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := workqueue.SetProgress(ctx, w.c.Pool, w.c.Schema, id, files); err != nil && ctx.Err() == nil {
			w.c.Logger.WarnContext(ctx, "media/video: report progress", "job", id, "error", err)
		}
	}
}
