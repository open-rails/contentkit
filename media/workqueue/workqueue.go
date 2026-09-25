// Package workqueue is the host's side of the media worker (media/worker):
// the River schema it drains in the host database, insert-only enqueueing and
// cancelling of its jobs, and processing progress for the read API. It needs
// neither ffmpeg nor libvips, so hosts that only presign, commit and read link
// it instead of the worker.
package workqueue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Each host puts its worker jobs in its own River schema, separate from the
// host's other jobs and from other hosts sharing the database.
const (
	Schema      = "media_worker" // default for the stock worker
	ImageQueue  = "media_image"  // image variants, slots, inline images; placement of staged images
	VideoQueue  = "media_video"  // encodes; placement of staged videos
	MaxAttempts = 5
)

// Migrate creates schema and applies River's migrations. Hosts run it in
// their migrate step; the worker also runs it at start.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if schema == "" {
		return errors.New("media/workqueue: schema is required")
	}
	return riverhelpers.ApplyMigrations(ctx, pool, schema)
}

// ImageArgs derives a ref's image variants, zip, slots and inline images, or
// one slot or inline image when Slot is set.
type ImageArgs struct {
	Ref   contentref.ContentRef `json:"ref"`
	Slot  string                `json:"slot,omitempty"`
	After int64                 `json:"after,omitempty"` // the running job this one follows
}

func (ImageArgs) Kind() string { return "contentkit_media_image" }

func (a ImageArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

// VideoArgs encodes a manifest's video files.
type VideoArgs struct {
	Ref contentref.ContentRef `json:"ref"`
}

func (VideoArgs) Kind() string { return "contentkit_media_video" }

// Queue is the host's insert-only client for the worker's jobs; it is the
// uploads' media.ProcessQueue.
type Queue struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	kinds  *media.Registry
}

var _ media.ProcessQueue = (*Queue)(nil)

func New(pool *pgxpool.Pool, kinds *media.Registry, schema string) (*Queue, error) {
	if pool == nil || kinds == nil || schema == "" {
		return nil, errors.New("media/workqueue: queue needs a pool, a registry and a schema")
	}
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		return nil, err
	}
	return &Queue{client: c, pool: pool, kinds: kinds}, nil
}

func jobTable(schema string) string { return pgx.Identifier{schema, "river_job"}.Sanitize() }

// Enqueue asks the worker to process job: an image job for kinds with image
// variants, slots or inline images (one pending job per ref and slot, with a
// follow-up behind a running one), and a video job for a video kind's
// manifest.
func (q *Queue) Enqueue(ctx context.Context, job media.ProcessJob) error {
	return q.enqueue(ctx, q.client.Insert, job)
}

// EnqueueTx enqueues in the host's transaction.
func (q *Queue) EnqueueTx(ctx context.Context, tx pgx.Tx, job media.ProcessJob) error {
	return q.enqueue(ctx, func(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
		return q.client.InsertTx(ctx, tx, args, o)
	}, job)
}

func (q *Queue) enqueue(ctx context.Context, insert media.InsertFunc, job media.ProcessJob) error {
	item, err := q.kinds.Item(job.Ref)
	if err != nil {
		return err
	}
	k := item.Kind()
	if job.Slot != "" || len(k.Specs) > 0 || len(k.Slots) > 0 || k.Inline != nil {
		if err := media.InsertOnce(ctx, insert, ImageArgs{Ref: job.Ref, Slot: job.Slot},
			river.InsertOpts{Queue: ImageQueue, MaxAttempts: MaxAttempts}); err != nil {
			return err
		}
	}
	if job.Slot != "" || k.Video == nil {
		return nil
	}
	if _, err := item.Section(); err != nil {
		return err
	}
	// Not unique: River's uniqueness always covers running jobs, which would
	// drop the job for a source replaced mid-encode. Duplicates serialize on
	// the worker's per-manifest lock and are no-ops once the manifest is fresh.
	_, err = insert(ctx, VideoArgs{Ref: job.Ref}, VideoInsertOpts())
	return err
}

// VideoInsertOpts are a video job's insert options.
func VideoInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{Queue: VideoQueue, MaxAttempts: MaxAttempts}
}

// Cancel cancels ref's queued and running image and video jobs, every stage:
// a running job's context is cancelled, so an encode is killed and publishes
// nothing further. It returns how many jobs it cancelled.
func (q *Queue) Cancel(ctx context.Context, ref contentref.ContentRef) (int, error) {
	match, version, err := RefMatch(ref)
	if err != nil {
		return 0, err
	}
	rows, err := q.pool.Query(ctx, `SELECT id FROM `+jobTable(q.client.Schema())+`
WHERE kind = ANY($1) AND args @> $2 AND args->'ref'->>'content_version_id' IS NOT DISTINCT FROM $3
  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`,
		[]string{ImageArgs{}.Kind(), VideoArgs{}.Kind()}, match, version)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := q.client.JobCancel(ctx, id); err != nil && !errors.Is(err, river.ErrNotFound) {
			return 0, err
		}
	}
	return len(ids), nil
}

// RefMatch is the jsonb containment and version a job query matches ref's jobs by.
func RefMatch(ref contentref.ContentRef) ([]byte, *string, error) {
	match, err := json.Marshal(map[string]any{"ref": map[string]string{
		"tenant_id": ref.TenantID, "content_kind": ref.ContentKind, "content_id": ref.ContentID}})
	if err != nil {
		return nil, nil, err
	}
	var version *string
	if v := ref.Version(); v != "" {
		version = &v
	}
	return match, version, nil
}

// Progress lives on the running video job's row (metadata.contentkit_progress):
// no extra table, it dies with the job, and a job River rescues from a dead
// worker stops being read as running.
const progressKey = "contentkit_progress"

// SetProgress records a running job's per-file progress (the worker's reports).
func SetProgress(ctx context.Context, pool *pgxpool.Pool, schema string, id int64, files map[string]media.EncodeProgress) error {
	b, err := json.Marshal(files)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `UPDATE `+jobTable(schema)+` SET metadata = jsonb_set(metadata, '{`+progressKey+`}', $2) WHERE id = $1 AND state = 'running'`, id, b)
	return err
}

// ClearProgress drops a finished job's progress.
func ClearProgress(ctx context.Context, pool *pgxpool.Pool, schema string, id int64) error {
	_, err := pool.Exec(ctx, `UPDATE `+jobTable(schema)+` SET metadata = metadata - '`+progressKey+`' WHERE id = $1`, id)
	return err
}

// stalledAfter marks a running job's progress stale: reports come at least
// every few seconds while ffmpeg or a transfer runs.
const stalledAfter = time.Minute

// NewProgressSource reads encode progress from the worker's video jobs, for
// media.ReaderOptions.Progress. One indexed query per read of an item with a
// pending video.
func NewProgressSource(pool *pgxpool.Pool, schema string) media.ProgressSource {
	return &progressSource{pool: pool, table: jobTable(schema), now: time.Now}
}

type progressSource struct {
	pool  *pgxpool.Pool
	table string
	now   func() time.Time
}

func (s *progressSource) EncodeProgress(ctx context.Context, ref contentref.ContentRef) (media.EncodeStatus, error) {
	var st media.EncodeStatus
	match, version, err := RefMatch(ref)
	if err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT j.state, j.metadata->'`+progressKey+`',
  CASE WHEN j.state = 'available' THEN 1 + (SELECT count(*) FROM `+s.table+` a
    WHERE a.state = 'available' AND a.queue = j.queue AND (a.priority, a.scheduled_at, a.id) < (j.priority, j.scheduled_at, j.id)) END
FROM `+s.table+` j
WHERE j.kind = $1 AND j.args @> $2 AND j.args->'ref'->>'content_version_id' IS NOT DISTINCT FROM $3
  AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
ORDER BY j.id`, VideoArgs{}.Kind(), match, version)
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
