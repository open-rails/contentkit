package video

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/workqueue"
)

const (
	chunkWindow = 2
)

type encodeRun struct {
	ID         string
	Ref        contentref.ContentRef
	File       string
	Source     string
	SourceKey  string
	SourceETag string
	Spec       string
	Rung       int
	Class      media.VideoJobClass
	State      string
	Probe      probeResult
}

type encodeChunk struct {
	Ordinal int
	StartMS int64
	EndMS   int64
	Output  map[string]string // rendition name to temporary object key
}

func (c WorkerConfig) runTable() string {
	return pgx.Identifier{c.Schema, "encode_run"}.Sanitize()
}

func (c WorkerConfig) chunkTable() string {
	return pgx.Identifier{c.Schema, "encode_chunk"}.Sanitize()
}

func (c WorkerConfig) jobTable() string {
	return pgx.Identifier{c.Schema, "river_job"}.Sanitize()
}

// planVideo only probes and records the work. It never downloads the source
// into scratch or runs an encode pass.
func (c WorkerConfig) planVideo(ctx context.Context, args workqueue.VideoPlanArgs) error {
	item, err := c.Kinds.Item(args.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	if item.Kind().Video == nil {
		return river.JobCancel(fmt.Errorf("media/video: kind %q has no video", item.Kind().Name))
	}
	release, ok, err := pglock.Acquire(ctx, c.Pool, "contentkit:media:video:"+args.Ref.String(), false)
	if err != nil {
		return err
	}
	if !ok {
		return river.JobSnooze(time.Minute)
	}
	defer release()

	ms, err := media.NewManifests(c.Encoder.c.Store, c.Kinds, media.ManifestOptions{Locker: c.Encoder.c.Locker,
		CacheSize: 1, Sweeps: c.Encoder.c.Sweeps})
	if err != nil {
		return err
	}
	man, _, err := ms.Get(ctx, args.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	recipe := c.Encoder.recipeOf(item.Kind().Video)
	for _, f := range man.Files {
		switch {
		case IsSubtitle(f) && !subtitleFresh(f):
			err = c.Encoder.subtitleFile(ctx, ms, item, f, nil)
		case IsVideo(f) && fresh(man, f, recipe) && subsStale(f):
			err = c.Encoder.sourceSubs(ctx, ms, item, item.Kind().Video, f, nil)
		case IsVideo(f) && !fresh(man, f, recipe):
			err = c.planFile(ctx, ms, item, recipe, f, args.Class)
		default:
			continue
		}
		var permanent *PermanentError
		if errors.As(err, &permanent) {
			err = c.Encoder.fail(ctx, ms, item, recipe.failSpec, f.Name, f.Source(), permanent.Err)
		}
		if err != nil {
			return fmt.Errorf("media/video: plan %s/%s: %w", args.Ref, f.Name, err)
		}
	}
	return nil
}

func (c WorkerConfig) planFile(ctx context.Context, ms *media.Manifests, item media.Item, r jobRecipe, f media.File, class media.VideoJobClass) error {
	source := f.Source()
	key, err := item.Original(source)
	if err != nil {
		return &PermanentError{err}
	}
	obj, err := c.Encoder.c.Store.Head(ctx, key)
	if errors.Is(err, media.ErrNotFound) {
		return c.Encoder.stale(ctx, ms, item, f.Name, source, err)
	} else if err != nil {
		return err
	}
	url, err := c.Encoder.c.Store.PresignGet(ctx, key, 2*time.Hour)
	if err != nil {
		return err
	}
	pr, err := probeRemote(ctx, url.URL)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PermanentError{err}
	}
	plan, err := newPlan(pr, r.video)
	if err != nil {
		return &PermanentError{err}
	}
	// A staged upload has no immutable key yet. Stream it once for its hash,
	// without storing a local copy, then atomically point the manifest at it.
	if layout.ValidStagedName(source) {
		body, got, err := c.Encoder.c.Store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			return err
		}
		h := sha256.New()
		n, readErr := io.Copy(h, body)
		err = errors.Join(readErr, body.Close())
		if err != nil {
			return err
		}
		if got.ETag != obj.ETag || n != obj.Size {
			return media.ErrPreconditionFailed
		}
		placed, err := ms.Place(ctx, item.Ref(), media.Staged{Name: source, ETag: obj.ETag, SHA256: h.Sum(nil)})
		if errors.Is(err, media.ErrStagedGone) {
			return c.Encoder.stale(ctx, ms, item, f.Name, source, err)
		} else if err != nil {
			return err
		}
		source = placed
		key, _ = item.Original(source)
		obj, err = c.Encoder.c.Store.Head(ctx, key)
		if err != nil {
			return err
		}
	}

	// Re-read after Place or another editor's manifest change. Planning a
	// superseded file is harmless but releasing work for it is not.
	current, _, err := ms.Get(ctx, item.Ref())
	if err != nil {
		return err
	}
	i := current.File(f.Name)
	if i < 0 || current.Files[i].Source() != source {
		return nil
	}
	stages := slices.Clone(plan.rungs)
	slices.Reverse(stages)
	done := c.Encoder.stagesDone(current.Files[i].HLS, source, r.spec, stages)
	return c.insertRuns(ctx, item.Ref(), f.Name, source, key, obj.ETag, r.spec, class, pr, plan, done)
}

// chunkBounds sizes work by output pixels and frames, then rounds boundaries
// to the HLS keyframe grid. A run always has at least one nonempty chunk.
func chunkBounds(p plan, rungs []rung, codecs int, target time.Duration) [][2]int64 {
	var pixelFactor float64
	for _, r := range rungs {
		pixelFactor += float64(r.w*r.h) / (1920 * 1080)
	}
	cost := math.Max(0.25, pixelFactor*float64(codecs)*p.fps/30)
	seconds := min(240.0, max(8.0, target.Seconds()/cost))
	step := max(int64(segmentSeconds*1000), int64(seconds/segmentSeconds)*segmentSeconds*1000)
	end := max(int64(1), int64(math.Ceil(p.duration*1000)))
	var bounds [][2]int64
	for start := int64(0); start < end; start += step {
		bounds = append(bounds, [2]int64{start, min(start+step, end)})
	}
	return bounds
}

func (c WorkerConfig) insertRuns(ctx context.Context, ref contentref.ContentRef, file, source, key, etag, spec string, class media.VideoJobClass, pr probeResult, p plan, done int) error {
	probeJSON, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	stages := slices.Clone(p.rungs)
	slices.Reverse(stages)
	previousComplete := true
	for i, stage := range stages {
		priority := workqueue.VideoPlanInsertOpts(class).Priority
		if i > 0 && priority == 1 {
			priority = 2
		}
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO `+c.runTable()+`
  (tenant_id, ref, file_name, source_name, source_key, source_etag, spec, rung, class, probe)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (ref, file_name, source_name, source_etag, spec, rung) DO NOTHING
RETURNING id::text`, ref.TenantID, refJSON, file, source, key, etag, spec, stage.n, string(class), probeJSON).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `SELECT id::text FROM `+c.runTable()+`
WHERE ref = $1 AND file_name = $2 AND source_name = $3 AND source_etag = $4 AND spec = $5 AND rung = $6`,
				refJSON, file, source, etag, spec, stage.n).Scan(&id); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if i >= done {
			for ordinal, bound := range chunkBounds(p, []rung{stage}, len(c.Encoder.c.Codecs), c.ChunkTarget) {
				if _, err := tx.Exec(ctx, `INSERT INTO `+c.chunkTable()+`
  (run_id, ordinal, start_ms, end_ms) VALUES ($1, $2, $3, $4)`, id, ordinal, bound[0], bound[1]); err != nil {
					return err
				}
			}
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM `+c.runTable()+` WHERE id = $1`, id).Scan(&state); err != nil {
			return err
		}
		if i < done && state != "complete" {
			if _, err := tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'complete', updated_at = now() WHERE id = $1`, id); err != nil {
				return err
			}
			state = "complete"
		} else if i >= done && (state == "cancelled" || state == "complete") {
			if _, err := tx.Exec(ctx, `UPDATE `+c.chunkTable()+`
SET state = 'planned', job_id = NULL, output = NULL, finished_at = NULL WHERE run_id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'planned', released = 0, completed = 0, class = $2, updated_at = now() WHERE id = $1`, id, string(class)); err != nil {
				return err
			}
			state = "planned"
		}
		if previousComplete {
			if state == "assembling" {
				var active bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+c.jobTable()+`
WHERE kind = $1 AND args->>'run_id' = $2 AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled'))`,
					(workqueue.VideoAssembleArgs{}).Kind(), id).Scan(&active); err != nil {
					return err
				}
				if !active {
					client := river.ClientFromContext[pgx.Tx](ctx)
					if client == nil {
						return errors.New("media/video: worker River client missing from context")
					}
					if _, err := client.InsertTx(ctx, tx, workqueue.VideoAssembleArgs{Ref: ref, RunID: id},
						&river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: priority, MaxAttempts: workqueue.VideoRiverMaxAttempts}); err != nil {
						return err
					}
				}
			} else if err := c.releaseWindow(ctx, tx, id, ref, priority); err != nil {
				return err
			}
		}
		previousComplete = state == "complete"
	}
	return tx.Commit(ctx)
}

// releaseWindow is called with its run row locked by the caller or by this
// SELECT. It never leaves more than chunkWindow active tasks for one run.
func (c WorkerConfig) releaseWindow(ctx context.Context, tx pgx.Tx, id string, ref contentref.ContentRef, priority int) error {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM `+c.runTable()+` WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
		return err
	}
	if state == "complete" || state == "cancelled" || state == "assembling" {
		return nil
	}
	// A cancelled or discarded River job no longer occupies the window.
	if _, err := tx.Exec(ctx, `UPDATE `+c.chunkTable()+` c SET state = 'planned', job_id = NULL
WHERE c.run_id = $1 AND c.state = 'queued' AND NOT EXISTS (
  SELECT 1 FROM `+c.jobTable()+` j WHERE j.id = c.job_id
    AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled'))`, id); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+c.chunkTable()+`
WHERE run_id = $1 AND state = 'queued'`, id).Scan(&active); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT ordinal FROM `+c.chunkTable()+`
WHERE run_id = $1 AND state = 'planned' ORDER BY ordinal LIMIT $2`, id, chunkWindow-active)
	if err != nil {
		return err
	}
	ordinals, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return err
	}
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		return errors.New("media/video: worker River client missing from context")
	}
	for _, ordinal := range ordinals {
		result, err := client.InsertTx(ctx, tx, workqueue.VideoChunkArgs{Ref: ref, RunID: id, Index: ordinal},
			&river.InsertOpts{Queue: workqueue.VideoEncodeQueue, Priority: priority, MaxAttempts: workqueue.VideoRiverMaxAttempts})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+c.chunkTable()+`
SET state = 'queued', job_id = $3 WHERE run_id = $1 AND ordinal = $2`, id, ordinal, result.Job.ID); err != nil {
			return err
		}
	}
	if len(ordinals) > 0 {
		_, err = tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'encoding', released = released + $2, updated_at = now() WHERE id = $1`, id, len(ordinals))
	}
	return err
}

func (c WorkerConfig) loadRun(ctx context.Context, id string) (encodeRun, error) {
	var run encodeRun
	var refJSON, probeJSON []byte
	err := c.Pool.QueryRow(ctx, `SELECT id::text, ref, file_name, source_name, source_key, source_etag,
  spec, rung, class, state, probe FROM `+c.runTable()+` WHERE id = $1`, id).Scan(
		&run.ID, &refJSON, &run.File, &run.Source, &run.SourceKey, &run.SourceETag,
		&run.Spec, &run.Rung, &run.Class, &run.State, &probeJSON)
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(refJSON, &run.Ref); err != nil {
		return run, err
	}
	return run, json.Unmarshal(probeJSON, &run.Probe)
}

func (c WorkerConfig) chunk(ctx context.Context, id string, index int) (encodeChunk, error) {
	var ch encodeChunk
	var raw []byte
	err := c.Pool.QueryRow(ctx, `SELECT ordinal, start_ms, end_ms, output FROM `+c.chunkTable()+`
WHERE run_id = $1 AND ordinal = $2`, id, index).Scan(&ch.Ordinal, &ch.StartMS, &ch.EndMS, &raw)
	if err != nil {
		return ch, err
	}
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &ch.Output)
	}
	return ch, err
}

func (c WorkerConfig) chunks(ctx context.Context, id string) ([]encodeChunk, error) {
	rows, err := c.Pool.Query(ctx, `SELECT ordinal, start_ms, end_ms, output FROM `+c.chunkTable()+`
WHERE run_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []encodeChunk
	for rows.Next() {
		var ch encodeChunk
		var raw []byte
		if err := rows.Scan(&ch.Ordinal, &ch.StartMS, &ch.EndMS, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &ch.Output); err != nil {
				return nil, err
			}
		}
		chunks = append(chunks, ch)
	}
	return chunks, rows.Err()
}
