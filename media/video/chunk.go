package video

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

func (c WorkerConfig) encodeChunk(ctx context.Context, job *river.Job[workqueue.VideoChunkArgs]) error {
	e := c.Encoder
	run, err := c.loadRun(ctx, job.Args.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if !run.Ref.Equal(job.Args.Ref) || run.State == "cancelled" || run.State == "complete" {
		return nil
	}
	if run.State != "encoding" {
		return river.JobSnooze(30 * time.Second)
	}
	ch, err := c.chunk(ctx, run.ID, job.Args.Index)
	if err != nil {
		return err
	}
	if ch.Output != nil {
		return c.completeChunk(ctx, job, run, ch)
	}
	fair, err := c.overTenantShare(ctx, run.Ref.TenantID)
	if err != nil {
		return err
	}
	if fair {
		return river.JobSnooze(30 * time.Second)
	}
	w, ok, err := c.current(ctx, run)
	if err != nil {
		return err
	}
	if !ok {
		return c.cancelRun(ctx, run.ID)
	}
	r := slices.IndexFunc(w.stages, func(r rung) bool { return r.n == run.Rung })
	if r < 0 {
		return c.cancelRun(ctx, run.ID)
	}
	url, err := e.store.PresignGet(ctx, run.SourceKey, 2*time.Hour)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	progress := newProgress(ctx, c.report(job.ID), e.c.ProgressInterval, time.Now, nil)
	fp := progress.file(run.File)
	defer progress.done(run.File)
	fp.probed(float64(ch.EndMS-ch.StartMS)/1000, dir)
	codecs := e.codecs(w.p)
	ps := pass{rung: w.stages[r], codecs: slices.Clone(codecs), noTracks: true,
		start: float64(ch.StartMS) / 1000, duration: float64(ch.EndMS-ch.StartMS) / 1000,
		enc: encoding{encoders: maps.Clone(e.encoders), threads: e.c.Threads, preset: e.c.Preset, topPreset: e.c.TopPreset,
			animation: profile(w.p) == media.VideoAnimation}, observe: e.c.ObserveEncode}
	// The top rung copies a compliant source, on the segments of the rung below.
	if run.Passthrough != "" && r > 0 && r == len(w.stages)-1 {
		lower, err := w.rendition(ctx, e, w.stages[r-1].n, run.Passthrough)
		if err != nil {
			return err
		}
		if lower != nil {
			name := renditionName(run.Rung, run.Passthrough)
			path := filepath.Join(dir, name+".mp4")
			copyErr := copyRungRemote(ctx, url.URL, dir, w.pl, name, run.Passthrough)
			valid := false
			if copyErr == nil && sameSegments(dir, name, lower.Segments) {
				if copied, err := probe(ctx, path); err == nil {
					if cp, err := newPlan(copied); err == nil {
						codec, ok, _ := passthroughable(ctx, path, cp, w.stages[r])
						valid = ok && codec == run.Passthrough
					}
				}
			}
			if valid {
				ps.codecs = slices.DeleteFunc(ps.codecs, func(codec media.Codec) bool { return codec == run.Passthrough })
			} else {
				if ctx.Err() != nil {
					return snoozeOnShutdown(ctx, ctx.Err())
				}
				e.c.Logger.DebugContext(ctx, "media/video: chunk passthrough unavailable", "run", run.ID, "chunk", ch.Ordinal, "error", copyErr)
				if err := removeRendition(dir, name); err != nil {
					return err
				}
			}
		}
	}
	if len(ps.codecs) > 0 {
		err = ladder(ctx, url.URL, dir, w.pl, ps, fp)
	}
	if err != nil && ctx.Err() == nil {
		cpu := false
		for codec, encoder := range ps.enc.encoders {
			if encoder == nvencEncoders[codec] {
				ps.enc.encoders[codec], cpu = cpuEncoders[codec], true
			}
		}
		if cpu {
			e.c.Logger.WarnContext(ctx, "media/video: NVENC failed; encoding on the CPU", "run", run.ID, "error", err)
			for _, codec := range ps.codecs {
				if err := removeRendition(dir, renditionName(run.Rung, codec)); err != nil {
					return err
				}
			}
			err = ladder(ctx, url.URL, dir, w.pl, ps, fp)
		}
	}
	if err != nil {
		return snoozeOnShutdown(ctx, err)
	}
	fp.set(media.PhaseUploading)
	output := make(map[string]string)
	for _, codec := range codecs {
		name := renditionName(run.Rung, codec)
		path := filepath.Join(dir, name+".mp4")
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if _, err := parsePlaylist(filepath.Join(dir, name+".m3u8"), st.Size()); err != nil {
			return err
		}
		key, err := e.putChunk(ctx, w.item, run.ID, ch.Ordinal, name, path)
		if err != nil {
			return snoozeOnShutdown(ctx, err)
		}
		output[name] = key
	}
	ch.Output = output
	return c.completeChunk(ctx, job, run, ch)
}

// runWork is a run's current state: its upload, preset and probed plan.
type runWork struct {
	item   media.Item
	man    *media.Manifest
	f      media.File
	p      *media.Private
	fp     string
	pl     plan
	stages []rung
}

// current loads a run's work; false when the run is superseded: the folder,
// the upload's blob or the preset's fingerprint changed, or the source is
// gone.
func (c WorkerConfig) current(ctx context.Context, run encodeRun) (runWork, bool, error) {
	e := c.Encoder
	var w runWork
	var err error
	if w.item, err = e.ms.Registry().Item(run.Ref); err != nil {
		return w, false, river.JobCancel(err)
	}
	w.man, _, err = e.ms.Get(ctx, run.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return w, false, nil
	} else if err != nil {
		return w, false, err
	}
	var ok bool
	if w.f, ok = w.man.Get(run.File); !ok || w.f.Blob != run.Source || w.f.Gone {
		return w, false, nil
	}
	if w.p, w.fp, ok = e.runPreset(w.item.Kind(), w.f, run.Spec); !ok {
		return w, false, nil
	}
	obj, err := e.store.Head(ctx, run.SourceKey)
	if errors.Is(err, media.ErrNotFound) || err == nil && obj.ETag != run.SourceETag {
		return w, false, nil
	} else if err != nil {
		return w, false, err
	}
	if w.pl, err = newPlan(run.Probe); err != nil {
		return w, false, river.JobCancel(err)
	}
	if w.stages, err = stages(w.p, w.pl); err != nil {
		return w, false, river.JobCancel(err)
	}
	return w, true, nil
}

// to is the preset's output path.
func (w runWork) to() string { return w.item.Kind().OutputPath(w.p, w.f.Path) }

// rendition is the published rendition of rung n in codec c with the
// run's fingerprint and its segments, or nil.
func (w runWork) rendition(ctx context.Context, e *Encoder, n int, c media.Codec) (*media.TrackIndex, error) {
	o, ok := w.man.Get(renditionPath(w.to(), n, c))
	if !ok || o.FP != w.fp || o.From != w.f.Path || o.Track == nil {
		return nil, nil
	}
	idx, err := e.readIndex(ctx, w.item, o.Track.Index)
	return &idx, err
}

func snoozeOnShutdown(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely) {
		return river.JobSnooze(0)
	}
	return err
}

func (c WorkerConfig) overTenantShare(ctx context.Context, tenant string) (bool, error) {
	var own, running int
	var otherWaiting bool
	err := c.Pool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM `+c.jobTable()+` WHERE kind = $1 AND state = 'running' AND args->'ref'->>'tenant_id' = $2),
  (SELECT count(*) FROM `+c.jobTable()+` WHERE kind = $1 AND state = 'running'),
  EXISTS (SELECT 1 FROM `+c.jobTable()+` WHERE kind = $1
    AND (state = 'available' OR state IN ('retryable', 'scheduled') AND scheduled_at <= now())
    AND args->'ref'->>'tenant_id' IS DISTINCT FROM $2)`,
		(workqueue.VideoChunkArgs{}).Kind(), tenant).Scan(&own, &running, &otherWaiting)
	if err != nil {
		return false, err
	}
	return otherWaiting && own > max(1, running/2), nil
}

// putChunk stores a chunk's rendition in the item's temp/ area until
// assembly; the sweep removes leftovers by age.
func (e *Encoder) putChunk(ctx context.Context, item media.Item, runID string, ordinal int, name, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	sum := h.Sum(nil)
	key := item.TempPrefix() + fmt.Sprintf("%s-%d-%s-%s.mp4", runID, ordinal, name, hex.EncodeToString(sum[:16]))
	if obj, err := e.store.Head(ctx, key); err == nil && obj.Size == size {
		return key, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", err
	}
	opts := media.PutOptions{ContentType: "video/mp4", ChecksumSHA256: sum}
	if e.store.Capabilities().ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	_, err = e.store.Put(ctx, key, io.NewSectionReader(f, 0, size), size, opts)
	if errors.Is(err, media.ErrPreconditionFailed) {
		return key, nil
	}
	return key, err
}

func (c WorkerConfig) completeChunk(ctx context.Context, job *river.Job[workqueue.VideoChunkArgs], run encodeRun, ch encodeChunk) error {
	output, err := json.Marshal(ch.Output)
	if err != nil {
		return err
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return snoozeOnShutdown(ctx, err)
	}
	defer tx.Rollback(ctx)
	var state string
	var priority int
	if err := tx.QueryRow(ctx, `SELECT r.state, CASE WHEN r.class = 'reencode' THEN 3 WHEN r.class = 'backfill' THEN 4
WHEN r.rung = (SELECT min(other.rung) FROM `+c.runTable()+` other
  WHERE other.ref = r.ref AND other.file_name = r.file_name AND other.source_name = r.source_name
    AND other.source_etag = r.source_etag AND other.spec = r.spec)
THEN 1 ELSE 2 END FROM `+c.runTable()+` r WHERE r.id = $1 FOR UPDATE`, run.ID).Scan(&state, &priority); err != nil {
		return err
	}
	if state == "cancelled" || state == "complete" {
		return nil
	}
	if state != "encoding" {
		return river.JobSnooze(30 * time.Second)
	}
	result, err := tx.Exec(ctx, `UPDATE `+c.chunkTable()+`
SET state = 'done', output = $4, finished_at = now()
WHERE run_id = $1 AND ordinal = $2 AND job_id = $3 AND state = 'queued'`, run.ID, ch.Ordinal, job.ID, output)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+c.chunkTable()+` WHERE run_id = $1 AND state != 'done'`, run.ID).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'assembling', completed = completed + 1, updated_at = now() WHERE id = $1`, run.ID); err != nil {
			return err
		}
		client := river.ClientFromContext[pgx.Tx](ctx)
		if _, err := client.InsertTx(ctx, tx, workqueue.VideoAssembleArgs{Ref: run.Ref, RunID: run.ID},
			&river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: priority, MaxAttempts: workqueue.VideoRiverMaxAttempts}); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET completed = completed + 1, updated_at = now() WHERE id = $1`, run.ID); err != nil {
			return err
		}
		if err := c.releaseWindow(ctx, tx, run.ID, run.Ref, priority); err != nil {
			return err
		}
	}
	if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
		return err
	}
	return snoozeOnShutdown(ctx, tx.Commit(ctx))
}

func (c WorkerConfig) cancelRun(ctx context.Context, id string) error {
	_, err := c.Pool.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'cancelled', updated_at = now() WHERE id = $1 AND state != 'complete'`, id)
	return err
}

// removeRendition removes a rendition's files from a failed pass.
func removeRendition(dir, v string) error {
	for _, f := range []string{v + ".mp4", v + ".m3u8"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
