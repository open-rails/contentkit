// Package workqueue is the host's side of the media worker (media/worker):
// the River schema it drains in the host database, insert-only enqueueing and
// cancelling of its jobs, and processing progress for the read API. It needs
// neither ffmpeg nor libvips, so hosts that only presign, commit and read link
// it instead of the worker.
package workqueue

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/migratekit"
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
	ImageQueue       = "media_image"        // image variants, slots and inline images
	VideoLightQueue  = "media_video_light"  // video probe, tracks and assembly
	VideoEncodeQueue = "media_video_encode" // bounded video chunks
	AudioQueue       = "media_audio"        // audio-only files
	MaxAttempts      = 5
	// River counts a rescued hard kill before Work can restore the attempt.
	// Video jobs enforce MaxAttempts on actual failures inside Work instead.
	VideoRiverMaxAttempts = 32767
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidSchema requires a lowercase Postgres identifier for the worker's schema.
func ValidSchema(schema string) error {
	if !schemaName.MatchString(schema) {
		return fmt.Errorf("media/workqueue: schema %q must be a lowercase identifier (e.g. \"doujins_media_worker\")", schema)
	}
	return nil
}

// Migrate creates the worker schema and applies River and video-run migrations.
// Hosts run it in their migrate step; the worker also runs it at start.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if err := ValidSchema(schema); err != nil {
		return err
	}
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		return err
	}
	scripts, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	baseline, err := migratekit.Load(scripts, ".", migratekit.RequireParentLinks())
	if err != nil {
		return fmt.Errorf("media/workqueue: load migrations: %w", err)
	}
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "contentkit-media-worker")
	if err != nil {
		return err
	}
	defer migrator.Close()
	if err := migrator.WithSchema(schema).ApplyMigrations(ctx, baseline); err != nil {
		return fmt.Errorf("media/workqueue: migrate %s: %w", schema, err)
	}
	return nil
}

// jobs is the schema's river_job table.
func jobs(schema string) string { return pgx.Identifier{schema, "river_job"}.Sanitize() }

// ImageArgs runs an item's image producers (Image and Zip presets, public
// presets, editor views): media.ProcessJob's fields.
type ImageArgs struct {
	Ref    contentref.ContentRef `json:"ref"`
	Preset string                `json:"preset,omitempty"`
	Force  bool                  `json:"force,omitempty"`
	Editor bool                  `json:"editor,omitempty"`
	After  int64                 `json:"after,omitempty"` // the running job this one follows
}

func (ImageArgs) Kind() string { return "contentkit_media_image" }

func (a ImageArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

// VideoPlanArgs runs an item's video producers (HLS, MP4, Subtitles
// presets and frames): it plans stale outputs into encode runs.
type VideoPlanArgs struct {
	Ref    contentref.ContentRef `json:"ref"`
	Preset string                `json:"preset,omitempty"`
	Force  bool                  `json:"force,omitempty"`
	Class  media.VideoJobClass   `json:"class,omitempty"`
}

func (VideoPlanArgs) Kind() string { return "contentkit_media_video" }

// VideoChunkArgs encodes one bounded range of a video run.
type VideoChunkArgs struct {
	Ref   contentref.ContentRef `json:"ref"`
	RunID string                `json:"run_id"`
	Index int                   `json:"index"`
}

func (VideoChunkArgs) Kind() string { return "contentkit_media_video_chunk" }

// VideoAssembleArgs publishes one rung after its chunks finish.
type VideoAssembleArgs struct {
	Ref   contentref.ContentRef `json:"ref"`
	RunID string                `json:"run_id"`
}

func (VideoAssembleArgs) Kind() string { return "contentkit_media_video_assemble" }

// AudioArgs runs an item's Audio presets.
type AudioArgs struct {
	Ref    contentref.ContentRef `json:"ref"`
	Preset string                `json:"preset,omitempty"`
	Force  bool                  `json:"force,omitempty"`
}

func (AudioArgs) Kind() string { return "contentkit_media_audio" }

// EncodeKinds are the job kinds that report encode progress.
var EncodeKinds = []string{VideoPlanArgs{}.Kind(), VideoChunkArgs{}.Kind(), VideoAssembleArgs{}.Kind(), AudioArgs{}.Kind()}

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

// Enqueue asks the worker to process job: one job per producer family the
// kind (or job.Preset) uses. Image jobs are one pending per job, with a
// follow-up behind a running one.
func (q *Queue) Enqueue(ctx context.Context, job media.ProcessJob) error {
	return q.enqueue(ctx, q.client.Insert, job)
}

// EnqueueTx enqueues in the host's transaction.
func (q *Queue) EnqueueTx(ctx context.Context, tx pgx.Tx, job media.ProcessJob) error {
	return q.enqueue(ctx, func(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
		return q.client.InsertTx(ctx, tx, args, o)
	}, job)
}

// Families are the producer families a kind's presets (or one preset) use.
type Families struct{ Image, Video, Audio bool }

// FamiliesOf maps k's presets (only preset, when set) to producer families.
func FamiliesOf(k *media.Kind, preset string) Families {
	var f Families
	for _, p := range k.Private {
		if preset != "" && p.Name != preset {
			continue
		}
		f.Image = f.Image || p.Image != nil || p.Zip != ""
		f.Video = f.Video || p.HLS != nil || p.MP4 != nil || p.Subtitles != nil
		f.Audio = f.Audio || p.Audio != nil
	}
	for _, p := range k.Public {
		f.Image = f.Image || preset == "" || p.Name == preset
	}
	for _, u := range k.Uploads {
		f.Video = f.Video || u.Frames != "" && preset == ""
	}
	return f
}

func (q *Queue) enqueue(ctx context.Context, insert media.InsertFunc, job media.ProcessJob) error {
	if job.Class != "" && job.Class != media.VideoReencode && job.Class != media.VideoBackfill {
		return fmt.Errorf("media/workqueue: invalid video job class %q", job.Class)
	}
	item, err := q.kinds.Item(job.Ref)
	if err != nil {
		return err
	}
	f := FamiliesOf(item.Kind(), job.Preset)
	if f.Image || job.Editor {
		if err := media.InsertOnce(ctx, insert, ImageArgs{Ref: job.Ref, Preset: job.Preset, Force: job.Force, Editor: job.Editor},
			river.InsertOpts{Queue: ImageQueue, MaxAttempts: MaxAttempts}); err != nil {
			return err
		}
	}
	// Not unique: River's uniqueness always covers running jobs, which would
	// drop the job for a source replaced mid-encode. Duplicates serialize on
	// the worker's per-manifest lock and are no-ops once the manifest is fresh.
	if f.Video {
		if _, err := insert(ctx, VideoPlanArgs{Ref: job.Ref, Preset: job.Preset, Force: job.Force, Class: job.Class}, VideoPlanInsertOpts(job.Class)); err != nil {
			return err
		}
	}
	if f.Audio {
		if _, err := insert(ctx, AudioArgs{Ref: job.Ref, Preset: job.Preset, Force: job.Force}, AudioInsertOpts()); err != nil {
			return err
		}
	}
	return nil
}

// AudioInsertOpts are an audio job's insert options.
func AudioInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{Queue: AudioQueue, MaxAttempts: MaxAttempts}
}

// VideoPlanInsertOpts are a video plan's insert options.
func VideoPlanInsertOpts(class media.VideoJobClass) *river.InsertOpts {
	priority := 1
	switch class {
	case media.VideoReencode:
		priority = 3
	case media.VideoBackfill:
		priority = 4
	}
	return &river.InsertOpts{Queue: VideoLightQueue, Priority: priority, MaxAttempts: VideoRiverMaxAttempts}
}

// Cancel cancels ref's queued and running image and video jobs, every stage:
// a running job's context is cancelled, so an encode is killed and publishes
// nothing further. It returns how many jobs it cancelled.
func (q *Queue) Cancel(ctx context.Context, ref contentref.ContentRef) (int, error) {
	match, err := RefMatch(ref)
	if err != nil {
		return 0, err
	}
	rows, err := q.pool.Query(ctx, `SELECT id FROM `+jobs(q.schema)+`
WHERE kind = ANY($1) AND args @> $2 AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`,
		append([]string{ImageArgs{}.Kind()}, EncodeKinds...), match)
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
	refJSON, err := json.Marshal(ref)
	if err != nil {
		return 0, err
	}
	_, err = q.pool.Exec(ctx, `UPDATE `+pgx.Identifier{q.schema, "encode_run"}.Sanitize()+`
SET state = 'cancelled', updated_at = now()
WHERE ref = $1 AND state IN ('planned', 'encoding', 'assembling')`, refJSON)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// RefMatch is the jsonb containment a job query matches ref's jobs by.
func RefMatch(ref contentref.ContentRef) ([]byte, error) {
	return json.Marshal(map[string]any{"ref": map[string]string{
		"tenant_id": ref.TenantID, "content_kind": ref.ContentKind, "content_id": ref.ContentID}})
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

// NewProgressSource reads job and per-rung progress from the host's worker
// schema for media.ReaderOptions.Progress.
func NewProgressSource(pool *pgxpool.Pool, schema string) (media.ProgressSource, error) {
	if err := ValidSchema(schema); err != nil {
		return nil, err
	}
	return &progressSource{pool: pool, now: time.Now, sql: `
SELECT j.state, j.metadata->'` + progressKey + `',
  CASE WHEN j.state = 'available' THEN 1 + (SELECT count(*) FROM ` + jobs(schema) + ` a
    WHERE a.state = 'available' AND a.queue = j.queue AND (a.priority, a.scheduled_at, a.id) < (j.priority, j.scheduled_at, j.id)) END
FROM ` + jobs(schema) + ` j
WHERE j.kind = ANY($1) AND j.args @> $2
  AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
ORDER BY j.id`, runSQL: `
SELECT DISTINCT ON (r.file_name) r.file_name, r.state,
  (SELECT count(*) FROM ` + pgx.Identifier{schema, "encode_run"}.Sanitize() + ` prior
    WHERE prior.ref = r.ref AND prior.file_name = r.file_name AND prior.source_name = r.source_name
      AND prior.source_etag = r.source_etag AND prior.spec = r.spec AND prior.rung <= r.rung),
  (SELECT count(*) FROM ` + pgx.Identifier{schema, "encode_run"}.Sanitize() + ` all_rungs
    WHERE all_rungs.ref = r.ref AND all_rungs.file_name = r.file_name AND all_rungs.source_name = r.source_name
      AND all_rungs.source_etag = r.source_etag AND all_rungs.spec = r.spec),
  COALESCE((SELECT sum(end_ms - start_ms) FROM ` + pgx.Identifier{schema, "encode_chunk"}.Sanitize() + `
    WHERE run_id = r.id AND state = 'done'), 0),
  COALESCE((SELECT max(end_ms) FROM ` + pgx.Identifier{schema, "encode_chunk"}.Sanitize() + `
    WHERE run_id = r.id), 0),
  EXISTS (SELECT 1 FROM ` + jobs(schema) + ` j WHERE j.kind = $2 AND j.args->>'run_id' = r.id::text AND j.state = 'running')
FROM ` + pgx.Identifier{schema, "encode_run"}.Sanitize() + ` r
WHERE r.ref = $1 AND r.state IN ('planned', 'encoding', 'assembling')
ORDER BY r.file_name, r.rung`}, nil
}

type progressSource struct {
	pool   *pgxpool.Pool
	now    func() time.Time
	sql    string
	runSQL string
}

func (s *progressSource) EncodeProgress(ctx context.Context, ref contentref.ContentRef) (media.EncodeStatus, error) {
	var st media.EncodeStatus
	match, err := RefMatch(ref)
	if err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx, s.sql, EncodeKinds, match)
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
			// The item's video and audio jobs may both be running; each
			// reports its own files.
			var files map[string]media.EncodeProgress
			if len(raw) > 0 && json.Unmarshal(raw, &files) == nil {
				for n, p := range files {
					if now.Sub(time.UnixMilli(p.At)) > stalledAfter {
						p.Stalled, p.ETA, p.Speed = true, 0, 0
					}
					if n == media.ItemProgressKey {
						if st.Item == nil {
							st.Item = &p
						}
					} else if _, ok := st.Files[n]; !ok {
						if st.Files == nil {
							st.Files = map[string]media.EncodeProgress{}
						}
						st.Files[n] = p
					}
				}
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
	if err := rows.Err(); err != nil {
		return st, err
	}
	rows.Close()
	refJSON, err := json.Marshal(ref)
	if err != nil {
		return st, err
	}
	runs, err := s.pool.Query(ctx, s.runSQL, refJSON, (VideoChunkArgs{}).Kind())
	if err != nil {
		return st, err
	}
	defer runs.Close()
	for runs.Next() {
		var name, state string
		var stage, stages, doneMS, totalMS int64
		var running bool
		if err := runs.Scan(&name, &state, &stage, &stages, &doneMS, &totalMS, &running); err != nil {
			return st, err
		}
		phase := media.PhaseQueued
		switch {
		case state == "assembling":
			phase = media.PhasePublishing
		case running:
			phase = media.PhaseEncoding
		}
		p := media.EncodeProgress{Phase: phase, At: now.UnixMilli(), Stage: int(stage), Stages: int(stages)}
		if totalMS > 0 {
			p.SegmentsTotal = int((totalMS + 3999) / 4000)
			p.SegmentsDone = min(p.SegmentsTotal, int(doneMS/4000))
			p.Percent = min(99, 100*float64(doneMS)/float64(totalMS))
		}
		if live, ok := st.Files[name]; ok {
			p.Phase, p.At, p.Speed, p.ETA, p.Stalled = live.Phase, live.At, live.Speed, live.ETA, live.Stalled
			if totalMS > 0 {
				activeMS := min(max(0, totalMS-doneMS), int64(live.SegmentsDone)*4000)
				p.SegmentsDone = min(p.SegmentsTotal, int((doneMS+activeMS)/4000))
				if doneMS+activeMS == totalMS {
					p.SegmentsDone = p.SegmentsTotal
				}
				p.Percent = min(99, 100*float64(doneMS+activeMS)/float64(totalMS))
				if live.Phase == media.PhaseEncoding && live.Speed > 0 {
					p.ETA = math.Ceil(float64(totalMS-doneMS-activeMS) / 1000 / live.Speed)
				}
			}
		}
		if st.Files == nil {
			st.Files = make(map[string]media.EncodeProgress)
		}
		st.Files[name] = p
	}
	return st, runs.Err()
}
