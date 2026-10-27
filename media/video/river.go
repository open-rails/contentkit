package video

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// WorkerConfig configures resumable video conversion.
type WorkerConfig struct {
	Encoder     *Encoder // its Manifests carry the host's registry
	Pool        *pgxpool.Pool
	Schema      string        // the host's worker schema (workqueue.ValidSchema)
	MaxWorkers  int           // concurrent video jobs per process; default 1
	ChunkTarget time.Duration // estimated CPU work per chunk; default 5 minutes
	Logger      *slog.Logger
}

func (c WorkerConfig) defaults() WorkerConfig {
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 1
	}
	if c.ChunkTarget <= 0 {
		c.ChunkTarget = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Run performs one resumable video step inside the item's derivative job.
// The media worker also runs image producers; no video River handler is registered.
func (c WorkerConfig) Run(ctx context.Context, job *river.Job[workqueue.DerivativeArgs]) error {
	c = c.defaults()
	defer c.clearProgress(ctx, job.ID)
	if err := c.Encoder.ms.Recover(ctx, job.Args.Ref); err != nil {
		return err
	}
	// The shared derivative worker owns retry exhaustion, outage snoozes, and
	// crash recovery for both image and video work.
	return c.convert(ctx, job)
}

// convert owns the whole conversion. Each attempt checkpoints at most one
// bounded chunk and publishes a finished rung before yielding the same job.
// The item lock prevents duplicate requests from encoding the same chunk.
func (c WorkerConfig) convert(ctx context.Context, job *river.Job[workqueue.DerivativeArgs]) error {
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

func (c WorkerConfig) clearProgress(ctx context.Context, id int64) {
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := workqueue.ClearProgress(clearCtx, c.Pool, c.Schema, id); err != nil {
		c.Logger.WarnContext(clearCtx, "media/video: clear progress", "job", id, "error", err)
	}
}

// lockedSnooze is how long a job waits while another job of its item holds
// the item's plan lock; the later job then finds the work done or
// does what the earlier one did not see.
const lockedSnooze = 5 * time.Second

func (c WorkerConfig) report(id int64) Report {
	return func(ctx context.Context, files map[string]media.EncodeProgress) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := workqueue.SetProgress(ctx, c.Pool, c.Schema, id, files); err != nil && ctx.Err() == nil {
			c.Logger.WarnContext(ctx, "media/video: report progress", "job", id, "error", err)
		}
	}
}
