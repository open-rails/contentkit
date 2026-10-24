package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// WorkerConfig configures resumable video conversion and audio jobs.
type WorkerConfig struct {
	Encoder *Encoder // its Manifests carry the host's registry
	Pool    *pgxpool.Pool
	Schema  string // the host's worker schema (workqueue.ValidSchema)
	// Timeout bounds an audio job. A video attempt checkpoints one bounded
	// range and has a one-hour timeout; audio keeps its 48-hour default.
	Timeout      time.Duration
	MaxWorkers   int           // concurrent video jobs per process; default 1 (ffmpeg uses every core)
	AudioWorkers int           // concurrent audio jobs per process; default 2
	ChunkTarget  time.Duration // estimated CPU work per chunk; default 5 minutes
	Logger       *slog.Logger
}

// Contribution registers the video and audio workers and queues for
// riverhelpers.New on a client with c.Schema.
func Contribution(c WorkerConfig) (riverhelpers.Contribution, error) {
	if c.Encoder == nil || c.Pool == nil {
		return riverhelpers.Contribution{}, errors.New("media/video: WorkerConfig needs an Encoder and a Pool")
	}
	if err := workqueue.ValidSchema(c.Schema); err != nil {
		return riverhelpers.Contribution{}, err
	}
	c = c.defaults()
	return riverhelpers.NewContribution("contentkit-media-video", func(_ context.Context, cfg *river.Config) error {
		if cfg.Schema != c.Schema {
			return fmt.Errorf("media/video: River schema must be %q, not %q", c.Schema, cfg.Schema)
		}
		for _, q := range []string{workqueue.VideoQueue, workqueue.AudioQueue} {
			if _, ok := cfg.Queues[q]; ok {
				return fmt.Errorf("media/video: queue %q already registered", q)
			}
		}
		cfg.Queues[workqueue.VideoQueue] = river.QueueConfig{MaxWorkers: c.MaxWorkers}
		cfg.Queues[workqueue.AudioQueue] = river.QueueConfig{MaxWorkers: c.AudioWorkers}
		if err := river.AddWorkerSafely(cfg.Workers, &videoWorker{c: c}); err != nil {
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

type videoWorker struct {
	river.WorkerDefaults[workqueue.VideoArgs]
	c WorkerConfig
}

func (w *videoWorker) Timeout(*river.Job[workqueue.VideoArgs]) time.Duration { return time.Hour }

func (w *videoWorker) Work(ctx context.Context, job *river.Job[workqueue.VideoArgs]) error {
	defer w.c.clearProgress(ctx, job.ID)
	return w.c.runVideoJob(ctx, job.JobRow, job.Args.Ref, func() error { return w.c.convert(ctx, job) })
}

// convert owns the whole conversion. Each attempt checkpoints at most one
// bounded chunk and publishes a finished rung before yielding the same job.
// The item lock prevents duplicate requests from encoding the same chunk.
func (c WorkerConfig) convert(ctx context.Context, job *river.Job[workqueue.VideoArgs]) error {
	release, ok, err := pglock.Acquire(ctx, c.Pool, "contentkit:media:video:"+job.Args.Ref.String(), false)
	if err != nil {
		return err
	}
	if !ok {
		return river.JobSnooze(lockedSnooze)
	}
	defer release()
	var metadata struct {
		Initialized bool `json:"contentkit_video_initialized"`
	}
	if err := json.Unmarshal(job.Metadata, &metadata); err != nil {
		return err
	}
	if !metadata.Initialized {
		if job.Args.Force {
			item, err := c.Encoder.ms.Registry().Item(job.Args.Ref)
			if err != nil {
				return river.JobCancel(err)
			}
			if err := c.Encoder.force(ctx, item, job.Args.Preset, videoFamily); err != nil {
				return err
			}
		}
		// No outputs are produced until this is durable, so a crash before it
		// cannot reset any successful work from this conversion.
		if _, err := c.Pool.Exec(ctx, `UPDATE `+c.jobTable()+`
   SET metadata = jsonb_set(metadata, '{contentkit_video_initialized}', 'true') WHERE id = $1`, job.ID); err != nil {
			return err
		}
	}
	if err := c.planVideo(ctx, job.Args); err != nil {
		return err
	}
	if fair, err := c.overTenantShare(ctx, job.Args.Ref.TenantID); err != nil {
		return err
	} else if fair {
		return river.JobSnooze(lockedSnooze)
	}
	ref, err := json.Marshal(job.Args.Ref)
	if err != nil {
		return err
	}
	rows, err := c.Pool.Query(ctx, `SELECT id::text FROM `+c.runTable()+`
 WHERE ref = $1 AND state IN ('planned', 'encoding', 'assembling') ORDER BY created_at, file_name, rung`, ref)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		run, err := c.loadRun(ctx, id)
		if err != nil {
			return err
		}
		if job.Args.Preset != "" && !strings.HasPrefix(run.Spec, job.Args.Preset+"@") {
			continue
		}
		if _, ok, err := c.current(ctx, run); err != nil {
			return err
		} else if !ok {
			if err := c.cancelRun(ctx, id); err != nil {
				return err
			}
			continue
		}
		chunk, pending, err := c.nextChunk(ctx, id)
		if err != nil {
			return err
		}
		if pending {
			if _, err := c.Pool.Exec(ctx, `UPDATE `+c.runTable()+` SET state = 'encoding', updated_at = now() WHERE id = $1 AND state != 'cancelled'`, id); err != nil {
				return err
			}
			if err := c.encodeChunk(ctx, run, chunk, job.ID); err != nil {
				return err
			}
			if err := c.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+c.chunkTable()+` WHERE run_id = $1 AND state = 'planned')`, id).Scan(&pending); err != nil {
				return err
			}
		}
		if !pending {
			if _, err := c.Pool.Exec(ctx, `UPDATE `+c.runTable()+` SET state = 'assembling', updated_at = now() WHERE id = $1 AND state != 'cancelled'`, id); err != nil {
				return err
			}
			if err := c.assemble(ctx, run, job.ID); err != nil {
				return err
			}
			// Initial playback has priority over improving a playable video's ladder.
			if job.Priority == 1 {
				finished, err := c.loadRun(ctx, id)
				if err != nil {
					return err
				}
				if finished.State == "complete" {
					if _, err := c.Pool.Exec(ctx, `UPDATE `+c.jobTable()+` SET priority = 2 WHERE id = $1`, job.ID); err != nil {
						return err
					}
				}
			}
		}
		// Yield even when this was the last rung: the next attempt confirms no
		// work remains, and a newly queued short video can run between chunks.
		return river.JobSnooze(0)
	}
	return nil
}

// store is the bucket the encoder writes, nil without an encoder.
func (c WorkerConfig) store() media.Store {
	if c.Encoder == nil {
		return nil
	}
	return c.Encoder.store
}

func (c WorkerConfig) clearProgress(ctx context.Context, id int64) {
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := workqueue.ClearProgress(clearCtx, c.Pool, c.Schema, id); err != nil {
		c.Logger.WarnContext(clearCtx, "media/video: clear progress", "job", id, "error", err)
	}
}

func (c WorkerConfig) runVideoJob(ctx context.Context, row *rivertype.JobRow, ref contentref.ContentRef, work func() error) error {
	if err := c.restoreRescuedAttempt(ctx, row); err != nil {
		return snoozeOnShutdown(ctx, err)
	}
	if row.Attempt > workqueue.MaxAttempts {
		return river.JobCancel(fmt.Errorf("media/video: %d failed attempts", row.Attempt-1))
	}
	err := media.SnoozeUnavailable(ctx, c.store(), row, snoozeOnShutdown(ctx, work()))
	if errors.Is(err, media.ErrManifestTooLarge) {
		return c.full(ctx, ref, err)
	}
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

// lockedSnooze is how long a job waits while another job of its item holds
// the item's plan or audio lock; the later job then finds the work done or
// does what the earlier one did not see.
const lockedSnooze = 5 * time.Second

type audioWorker struct {
	river.WorkerDefaults[workqueue.AudioArgs]
	c WorkerConfig
}

func (w *audioWorker) Timeout(*river.Job[workqueue.AudioArgs]) time.Duration { return w.c.Timeout }

// Work produces the item's stale Audio presets under the item's audio lock,
// so a video encode of the same item does not hold it back.
func (w *audioWorker) Work(ctx context.Context, job *river.Job[workqueue.AudioArgs]) (err error) {
	defer func() { err = media.SnoozeUnavailable(ctx, w.c.store(), job.JobRow, err) }()
	item, err := w.c.Encoder.ms.Registry().Item(job.Args.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	// The lock's own connection lives outside Pool.
	release, ok, err := pglock.Acquire(ctx, w.c.Pool, "contentkit:media:audio:"+job.Args.Ref.String(), false)
	if err != nil {
		return err
	}
	if !ok {
		return river.JobSnooze(lockedSnooze)
	}
	defer release()
	defer w.c.clearProgress(ctx, job.ID)
	if err = w.c.Encoder.audio(ctx, item, job.Args, w.c.report(job.ID)); errors.Is(err, media.ErrManifestTooLarge) {
		return w.c.full(ctx, job.Args.Ref, err)
	}
	return err
}

// full marks the item Full (its outputs do not fit) and ends the job: a
// retry would only be refused again.
func (c WorkerConfig) full(ctx context.Context, ref contentref.ContentRef, err error) error {
	return river.JobCancel(errors.Join(err, c.Encoder.ms.SetFull(ctx, ref, err)))
}

func (c WorkerConfig) report(id int64) Report {
	return func(ctx context.Context, files map[string]media.EncodeProgress) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := workqueue.SetProgress(ctx, c.Pool, c.Schema, id, files); err != nil && ctx.Err() == nil {
			c.Logger.WarnContext(ctx, "media/video: report progress", "job", id, "error", err)
		}
	}
}
