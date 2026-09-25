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
	"fmt"
	"regexp"
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

// The worker's jobs live in their own River schema in the host database, so
// the heavy worker never joins (or wins leadership of) the host's River
// client. Each host names its schema (e.g. "doujins_media_worker"): hosts
// sharing a database must not share one, or one host's worker takes the
// other's jobs. Queue names are fixed within a schema.
const (
	ImageQueue  = "media_image" // image variants, slots, inline images; placement of staged images
	VideoQueue  = "media_video" // encodes; placement of staged videos
	MaxAttempts = 5
)

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidSchema requires a lowercase Postgres identifier for the worker's schema.
func ValidSchema(schema string) error {
	if !schemaName.MatchString(schema) {
		return fmt.Errorf("media/workqueue: schema %q must be a lowercase identifier (e.g. \"doujins_media_worker\")", schema)
	}
	return nil
}

// Migrate creates schema and applies River's migrations. Hosts run it in
// their migrate step; the worker also runs it at start.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if err := ValidSchema(schema); err != nil {
		return err
	}
	return riverhelpers.ApplyMigrations(ctx, pool, schema)
}

// jobs is the schema's river_job table.
func jobs(schema string) string { return pgx.Identifier{schema, "river_job"}.Sanitize() }

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
	schema string
}

var _ media.ProcessQueue = (*Queue)(nil)

// New is the host's queue into its worker schema (see ValidSchema).
func New(pool *pgxpool.Pool, kinds *media.Registry, schema string) (*Queue, error) {
	if pool == nil || kinds == nil {
		return nil, errors.New("media/workqueue: Queue needs a pool and a Registry")
	}
	if err := ValidSchema(schema); err != nil {
		return nil, err
	}
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		return nil, err
	}
	return &Queue{client: c, pool: pool, kinds: kinds, schema: schema}, nil
}

// Schema is the worker schema the queue inserts into.
func (q *Queue) Schema() string { return q.schema }

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
	rows, err := q.pool.Query(ctx, `SELECT id FROM `+jobs(q.schema)+`
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
	_, err = pool.Exec(ctx, `UPDATE `+jobs(schema)+` SET metadata = jsonb_set(metadata, '{`+progressKey+`}', $2) WHERE id = $1 AND state = 'running'`, id, b)
	return err
}

// ClearProgress drops a finished job's progress.
func ClearProgress(ctx context.Context, pool *pgxpool.Pool, schema string, id int64) error {
	_, err := pool.Exec(ctx, `UPDATE `+jobs(schema)+` SET metadata = metadata - '`+progressKey+`' WHERE id = $1`, id)
	return err
}

// stalledAfter marks a running job's progress stale: reports come at least
// every few seconds while ffmpeg or a transfer runs.
const stalledAfter = time.Minute

// NewProgressSource reads encode progress from the video jobs in the host's
// worker schema, for media.ReaderOptions.Progress. One indexed query per read
// of an item with a pending video.
func NewProgressSource(pool *pgxpool.Pool, schema string) (media.ProgressSource, error) {
	if err := ValidSchema(schema); err != nil {
		return nil, err
	}
	return &progressSource{pool: pool, now: time.Now, sql: `
SELECT j.state, j.metadata->'` + progressKey + `',
  CASE WHEN j.state = 'available' THEN 1 + (SELECT count(*) FROM ` + jobs(schema) + ` a
    WHERE a.state = 'available' AND a.queue = j.queue AND (a.priority, a.scheduled_at, a.id) < (j.priority, j.scheduled_at, j.id)) END
FROM ` + jobs(schema) + ` j
WHERE j.kind = $1 AND j.args @> $2 AND j.args->'ref'->>'content_version_id' IS NOT DISTINCT FROM $3
  AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
ORDER BY j.id`}, nil
}

type progressSource struct {
	pool *pgxpool.Pool
	now  func() time.Time
	sql  string
}

func (s *progressSource) EncodeProgress(ctx context.Context, ref contentref.ContentRef) (media.EncodeStatus, error) {
	var st media.EncodeStatus
	match, version, err := RefMatch(ref)
	if err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx, s.sql, VideoArgs{}.Kind(), match, version)
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
