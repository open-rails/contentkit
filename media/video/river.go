package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pglock"
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

func followUpOpts() *river.InsertOpts {
	o := insertOpts()
	o.Priority = 2
	return o
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

// Work runs one encode stage under a per-manifest lock; a duplicate job for
// a manifest being encoded waits by snoozing. A file left with a second
// stage gets a follow-up job at a lower priority, inserted after the lock is
// released, so other uploads' first stages run before it.
func (w *worker) Work(ctx context.Context, job *river.Job[Args]) (err error) {
	var more bool
	defer func() {
		if err == nil && more {
			_, err = river.ClientFromContext[pgx.Tx](ctx).Insert(ctx, job.Args, followUpOpts())
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
		if _, err := w.c.Pool.Exec(ctx, clearProgressSQL, job.ID); err != nil {
			w.c.Logger.WarnContext(ctx, "media/video: clear progress", "job", job.ID, "error", err)
		}
	}()
	more, err = w.c.Encoder.encode(ctx, Job(job.Args), w.report(job.ID), true)
	var perm *PermanentError
	if errors.As(err, &perm) {
		w.c.Logger.ErrorContext(ctx, "media/video: cannot encode", "ref", job.Args.Ref.String(), "error", err)
		return river.JobCancel(err)
	}
	return err
}

// Progress lives on the running job's row (metadata.contentkit_progress):
// no extra table, it dies with the job, and a job River rescues from a dead
// worker stops being read as running.
const (
	progressKey      = "contentkit_progress"
	setProgressSQL   = `UPDATE ` + Schema + `.river_job SET metadata = jsonb_set(metadata, '{` + progressKey + `}', $2) WHERE id = $1 AND state = 'running'`
	clearProgressSQL = `UPDATE ` + Schema + `.river_job SET metadata = metadata - '` + progressKey + `' WHERE id = $1`
)

func (w *worker) report(id int64) Report {
	return func(ctx context.Context, files map[string]media.EncodeProgress) {
		b, err := json.Marshal(files)
		if err == nil {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err = w.c.Pool.Exec(ctx, setProgressSQL, id, b)
		}
		if err != nil && ctx.Err() == nil {
			w.c.Logger.WarnContext(ctx, "media/video: report progress", "job", id, "error", err)
		}
	}
}

// stalledAfter marks a running job's progress stale: reports come at least
// every few seconds while ffmpeg or a transfer runs.
const stalledAfter = time.Minute

// NewProgressSource reads encode progress from the video jobs in the host
// database, for media.ReaderOptions.Progress. One indexed query per read of
// an item with a pending video.
func NewProgressSource(pool *pgxpool.Pool) media.ProgressSource {
	return &progressSource{pool: pool, now: time.Now}
}

type progressSource struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

const progressSQL = `
SELECT j.state, j.metadata->'` + progressKey + `',
  CASE WHEN j.state = 'available' THEN 1 + (SELECT count(*) FROM ` + Schema + `.river_job a
    WHERE a.state = 'available' AND a.queue = j.queue AND (a.priority, a.scheduled_at, a.id) < (j.priority, j.scheduled_at, j.id)) END
FROM ` + Schema + `.river_job j
WHERE j.kind = $1 AND j.args @> $2 AND j.args->'ref'->>'content_version_id' IS NOT DISTINCT FROM $3
  AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
ORDER BY j.id`

func (s *progressSource) EncodeProgress(ctx context.Context, ref contentref.ContentRef) (media.EncodeStatus, error) {
	var st media.EncodeStatus
	match, err := json.Marshal(map[string]any{"ref": map[string]string{
		"tenant_id": ref.TenantID, "content_kind": ref.ContentKind, "content_id": ref.ContentID}})
	if err != nil {
		return st, err
	}
	var version *string
	if v := ref.Version(); v != "" {
		version = &v
	}
	rows, err := s.pool.Query(ctx, progressSQL, Args{}.Kind(), match, version)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	now := s.now()
	for rows.Next() {
		var state string
		var raw []byte
		var position *int64
		if err := rows.Scan(&state, &raw, &position); err != nil {
			return st, err
		}
		if state == "running" {
			var files map[string]media.EncodeProgress
			if len(raw) > 0 && json.Unmarshal(raw, &files) == nil && st.Files == nil {
				for n, p := range files {
					if now.Sub(time.UnixMilli(p.At)) > stalledAfter {
						p.Stalled, p.ETA, p.Speed = true, 0, 0
					}
					if n == media.ItemProgressKey {
						st.Item = &p
						delete(files, n)
					} else {
						files[n] = p
					}
				}
				st.Files = files
			}
			continue
		}
		q := media.EncodeProgress{Phase: media.PhaseQueued, At: now.UnixMilli()}
		if position != nil {
			q.QueuePosition = int(*position)
		}
		if st.Queued == nil || q.QueuePosition > 0 && (st.Queued.QueuePosition == 0 || q.QueuePosition < st.Queued.QueuePosition) {
			st.Queued = &q
		}
	}
	return st, rows.Err()
}
