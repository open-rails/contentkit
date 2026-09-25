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

	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// WorkerConfig configures the River side of the media worker's video and
// audio jobs (workqueue.VideoPlanArgs on workqueue.VideoLightQueue, workqueue.AudioArgs
// on workqueue.AudioQueue, so audio never waits behind hours of video).
type WorkerConfig struct {
	Encoder *Encoder
	Pool    *pgxpool.Pool
	Schema  string // the host's worker schema (workqueue.ValidSchema)
	// Kinds is the host's registry: a job names only its ref, and the encode
	// takes the kind's ladder and bounds from here.
	Kinds *media.Registry
	// Timeout bounds one job. It must exceed the worst case: a 2 h 4K ladder on
	// 2 CPU runs for many hours. Default 48 h; River rescues a job stuck an
	// hour past it.
	Timeout      time.Duration
	MaxWorkers   int // concurrent video jobs per process; default 1 (ffmpeg uses every core)
	AudioWorkers int // concurrent audio jobs per process; default 2
	Logger       *slog.Logger
}

// Contribution registers the video and audio workers and queues for
// riverhelpers.New on a client with c.Schema.
func Contribution(c WorkerConfig) (riverhelpers.Contribution, error) {
	if c.Encoder == nil || c.Pool == nil || c.Kinds == nil {
		return riverhelpers.Contribution{}, errors.New("media/video: WorkerConfig needs an Encoder, a Pool and Kinds")
	}
	if err := workqueue.ValidSchema(c.Schema); err != nil {
		return riverhelpers.Contribution{}, err
	}
	c = c.defaults()
	return riverhelpers.NewContribution("contentkit-media-video", func(_ context.Context, cfg *river.Config) error {
		if cfg.Schema != c.Schema {
			return fmt.Errorf("media/video: River schema must be %q, not %q", c.Schema, cfg.Schema)
		}
		if cfg.JobTimeout < c.Timeout {
			return fmt.Errorf("media/video: client JobTimeout %s is below the video timeout %s", cfg.JobTimeout, c.Timeout)
		}
		for _, q := range []string{workqueue.VideoLightQueue, workqueue.AudioQueue} {
			if _, ok := cfg.Queues[q]; ok {
				return fmt.Errorf("media/video: queue %q already registered", q)
			}
		}
		cfg.Queues[workqueue.VideoLightQueue] = river.QueueConfig{MaxWorkers: c.MaxWorkers}
		cfg.Queues[workqueue.AudioQueue] = river.QueueConfig{MaxWorkers: c.AudioWorkers}
		if err := river.AddWorkerSafely(cfg.Workers, &worker{c: c}); err != nil {
			return err
		}
		return river.AddWorkerSafely(cfg.Workers, &audioWorker{c: c})
	}, nil, nil), nil
}

func (c WorkerConfig) defaults() WorkerConfig {
	if c.Timeout <= 0 {
		c.Timeout = 48 * time.Hour
	}
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 1
	}
	if c.AudioWorkers <= 0 {
		c.AudioWorkers = 2
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// ClientConfig is a River client configuration running only c's video and audio jobs.
func ClientConfig(c WorkerConfig) *river.Config {
	c = c.defaults()
	return &river.Config{Schema: c.Schema, JobTimeout: c.Timeout, Logger: c.Logger}
}

type worker struct {
	river.WorkerDefaults[workqueue.VideoPlanArgs]
	c WorkerConfig
}

func (w *worker) Timeout(*river.Job[workqueue.VideoPlanArgs]) time.Duration { return w.c.Timeout }

// Work runs one encode stage under a per-manifest lock; a duplicate job for
// a manifest being encoded waits by snoozing. A file left with a second
// stage gets a follow-up job at a lower priority, inserted after the lock is
// released, so other uploads' first stages run before it.
func (w *worker) Work(ctx context.Context, job *river.Job[workqueue.VideoPlanArgs]) (err error) {
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
			o := workqueue.VideoPlanInsertOpts(job.Args.Class)
			o.Priority = 2
			_, err = river.ClientFromContext[pgx.Tx](ctx).Insert(ctx, job.Args, o)
		}
	}()
	k := item.Kind()
	return w.c.run(ctx, job.ID, "video", Job{Ref: job.Args.Ref, Versioned: k.Versioned, Video: *k.Video, only: encodeVideo}, &more)
}

type audioWorker struct {
	river.WorkerDefaults[workqueue.AudioArgs]
	c WorkerConfig
}

func (w *audioWorker) Timeout(*river.Job[workqueue.AudioArgs]) time.Duration { return w.c.Timeout }

// Work encodes the manifest's stale audio files under its own per-manifest
// lock, so a video encode of the same item does not hold it back.
func (w *audioWorker) Work(ctx context.Context, job *river.Job[workqueue.AudioArgs]) error {
	item, err := w.c.Kinds.Item(job.Args.Ref)
	if err == nil && item.Kind().Audio == nil {
		err = fmt.Errorf("media/video: kind %q has no audio", item.Kind().Name)
	}
	if err != nil {
		return river.JobCancel(err)
	}
	k := item.Kind()
	return w.c.run(ctx, job.ID, "audio", Job{Ref: job.Args.Ref, Versioned: k.Versioned, Audio: k.Audio, only: encodeAudio}, nil)
}

// run encodes job under the per-manifest lock of its kind of work, clearing
// the job's progress after; more, when set, reports a stage left to run.
func (c WorkerConfig) run(ctx context.Context, id int64, work string, job Job, more *bool) (err error) {
	// The lock's own connection lives outside Pool, which the encode uses.
	release, ok, err := pglock.Acquire(ctx, c.Pool, "contentkit:media:"+work+":"+job.Ref.String(), false)
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
		if err := workqueue.ClearProgress(ctx, c.Pool, c.Schema, id); err != nil {
			c.Logger.WarnContext(ctx, "media/video: clear progress", "job", id, "error", err)
		}
	}()
	m, err := c.Encoder.encode(ctx, job, c.report(id), true)
	if more != nil {
		*more = m
	}
	var perm *PermanentError
	if errors.As(err, &perm) {
		c.Logger.ErrorContext(ctx, "media/video: cannot encode", "ref", job.Ref.String(), "error", err)
		return river.JobCancel(err)
	}
	return err
}

func (w WorkerConfig) report(id int64) Report {
	return func(ctx context.Context, files map[string]media.EncodeProgress) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := workqueue.SetProgress(ctx, w.Pool, w.Schema, id, files); err != nil && ctx.Err() == nil {
			w.Logger.WarnContext(ctx, "media/video: report progress", "job", id, "error", err)
		}
	}
}
