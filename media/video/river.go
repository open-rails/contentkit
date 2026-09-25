package video

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// WorkerConfig configures the River side of video planning, bounded encoding,
// assembly and audio jobs.
type WorkerConfig struct {
	Encoder *Encoder
	Pool    *pgxpool.Pool
	Schema  string // the host's worker schema (workqueue.ValidSchema)
	// Kinds is the host's registry: a job names only its ref, and the encode
	// takes the kind's ladder and bounds from here.
	Kinds *media.Registry
	// Timeout bounds an audio-only job. Video chunks, planning and assembly
	// each have a one-hour timeout; audio keeps its 48-hour default.
	Timeout      time.Duration
	MaxWorkers   int           // concurrent video jobs per process; default 1 (ffmpeg uses every core)
	AudioWorkers int           // concurrent audio jobs per process; default 2
	ChunkTarget  time.Duration // estimated CPU work per chunk; default 5 minutes
	Queue        string        // empty for all queues; light also handles audio, encode only chunks
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
	if c.Queue != "" && c.Queue != workqueue.VideoLightQueue && c.Queue != workqueue.VideoEncodeQueue {
		return riverhelpers.Contribution{}, fmt.Errorf("media/video: unsupported queue %q", c.Queue)
	}
	c = c.defaults()
	return riverhelpers.NewContribution("contentkit-media-video", func(_ context.Context, cfg *river.Config) error {
		if cfg.Schema != c.Schema {
			return fmt.Errorf("media/video: River schema must be %q, not %q", c.Schema, cfg.Schema)
		}
		for _, q := range []string{workqueue.VideoLightQueue, workqueue.VideoEncodeQueue, workqueue.AudioQueue} {
			if c.Queue != "" && c.Queue != q && !(c.Queue == workqueue.VideoLightQueue && q == workqueue.AudioQueue) {
				continue
			}
			if _, ok := cfg.Queues[q]; ok {
				return fmt.Errorf("media/video: queue %q already registered", q)
			}
		}
		if c.Queue == "" || c.Queue == workqueue.VideoLightQueue {
			cfg.Queues[workqueue.VideoLightQueue] = river.QueueConfig{MaxWorkers: c.MaxWorkers}
		}
		if c.Queue == "" || c.Queue == workqueue.VideoEncodeQueue {
			cfg.Queues[workqueue.VideoEncodeQueue] = river.QueueConfig{MaxWorkers: c.MaxWorkers}
		}
		if c.Queue == "" || c.Queue == workqueue.VideoLightQueue {
			cfg.Queues[workqueue.AudioQueue] = river.QueueConfig{MaxWorkers: c.AudioWorkers}
		}
		if err := river.AddWorkerSafely(cfg.Workers, &planWorker{c: c}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &chunkWorker{c: c}); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(cfg.Workers, &assembleWorker{c: c}); err != nil {
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
	if c.ChunkTarget <= 0 {
		c.ChunkTarget = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// ClientConfig is a River client configuration running only c's video and audio jobs.
func ClientConfig(c WorkerConfig) *river.Config {
	c = c.defaults()
	return &river.Config{Schema: c.Schema, JobTimeout: time.Hour,
		RescueStuckJobsAfter: 2 * time.Hour, Logger: c.Logger}
}

type planWorker struct {
	river.WorkerDefaults[workqueue.VideoPlanArgs]
	c WorkerConfig
}

func (w *planWorker) Timeout(*river.Job[workqueue.VideoPlanArgs]) time.Duration { return time.Hour }

func (w *planWorker) Work(ctx context.Context, job *river.Job[workqueue.VideoPlanArgs]) error {
	return w.c.runVideoJob(ctx, job.JobRow, func() error { return w.c.planVideo(ctx, job.Args) })
}

type chunkWorker struct {
	river.WorkerDefaults[workqueue.VideoChunkArgs]
	c WorkerConfig
}

func (w *chunkWorker) Timeout(*river.Job[workqueue.VideoChunkArgs]) time.Duration { return time.Hour }

func (w *chunkWorker) Work(ctx context.Context, job *river.Job[workqueue.VideoChunkArgs]) error {
	return w.c.runVideoJob(ctx, job.JobRow, func() error { return w.c.encodeChunk(ctx, job) })
}

type assembleWorker struct {
	river.WorkerDefaults[workqueue.VideoAssembleArgs]
	c WorkerConfig
}

func (w *assembleWorker) Timeout(*river.Job[workqueue.VideoAssembleArgs]) time.Duration {
	return time.Hour
}

func (w *assembleWorker) Work(ctx context.Context, job *river.Job[workqueue.VideoAssembleArgs]) error {
	return w.c.runVideoJob(ctx, job.JobRow, func() error { return w.c.assemble(ctx, job.Args) })
}

func (c WorkerConfig) runVideoJob(ctx context.Context, row *rivertype.JobRow, work func() error) error {
	if err := c.restoreRescuedAttempt(ctx, row); err != nil {
		return snoozeOnShutdown(ctx, err)
	}
	if row.Attempt > workqueue.MaxAttempts {
		return river.JobCancel(fmt.Errorf("media/video: %d failed attempts", row.Attempt-1))
	}
	err := snoozeOnShutdown(ctx, work())
	var snooze *river.JobSnoozeError
	var cancelled *river.JobCancelError
	if err != nil && row.Attempt >= workqueue.MaxAttempts && !errors.As(err, &snooze) && !errors.As(err, &cancelled) {
		return river.JobCancel(err)
	}
	return err
}

// River records a rescued hard-killed job as an error and keeps its attempt.
// Only failures returned by a worker should consume the bounded retry budget.
func (c WorkerConfig) restoreRescuedAttempt(ctx context.Context, job *rivertype.JobRow) error {
	failures := 0
	for _, attempt := range job.Errors {
		if attempt.Error != "Stuck job rescued by JobRescuer" {
			failures++
		}
	}
	want := failures + 1
	if job.Attempt <= want {
		return nil
	}
	result, err := c.Pool.Exec(ctx, `UPDATE `+c.jobTable()+`
SET attempt = $2 WHERE id = $1 AND state = 'running' AND attempt = $3`, job.ID, want, job.Attempt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("media/video: job %d changed while restoring rescued attempt", job.ID)
	}
	job.Attempt = want
	return nil
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

// run encodes an audio job under its per-manifest lock and clears progress.
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
