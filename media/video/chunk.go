package video

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	item, err := c.Kinds.Item(run.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	if item.Kind().Video == nil || c.Encoder.Spec(*item.Kind().Video) != run.Spec {
		return c.cancelRun(ctx, run.ID)
	}
	manifests, err := media.NewManifests(c.Encoder.c.Store, c.Kinds, media.ManifestOptions{Locker: c.Encoder.c.Locker, CacheSize: 1})
	if err != nil {
		return err
	}
	man, _, err := manifests.Get(ctx, run.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return c.cancelRun(ctx, run.ID)
	} else if err != nil {
		return err
	}
	if i := man.File(run.File); i < 0 || man.Files[i].Source() != run.Source {
		return c.cancelRun(ctx, run.ID)
	}
	obj, err := c.Encoder.c.Store.Head(ctx, run.SourceKey)
	if errors.Is(err, media.ErrNotFound) || err == nil && obj.ETag != run.SourceETag {
		return c.cancelRun(ctx, run.ID)
	} else if err != nil {
		return err
	}
	p, err := newPlan(run.Probe, item.Kind().Video)
	if err != nil {
		return river.JobCancel(err)
	}
	url, err := c.Encoder.c.Store.PresignGet(ctx, run.SourceKey, 2*time.Hour)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(c.Encoder.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	output := make(map[string]string)
	found := false
	for _, r := range p.rungs {
		if r.n != run.Rung {
			continue
		}
		found = true
		ps := pass{rung: r, codecs: slices.Clone(c.Encoder.c.Codecs), noTracks: true,
			start: float64(ch.StartMS) / 1000, duration: float64(ch.EndMS-ch.StartMS) / 1000,
			enc: encoding{encoders: maps.Clone(c.Encoder.encoders), threads: c.Encoder.c.Threads,
				preset: c.Encoder.c.Preset, topPreset: c.Encoder.c.TopPreset,
				animation: item.Kind().Video.Profile == media.VideoAnimation}, observe: c.Encoder.c.ObserveEncode}
		err := ladder(ctx, url.URL, dir, p, ps, nil)
		if err != nil && ctx.Err() == nil {
			cpu := false
			for codec, encoder := range ps.enc.encoders {
				if encoder == nvencEncoders[codec] {
					ps.enc.encoders[codec], cpu = cpuEncoders[codec], true
				}
			}
			if cpu {
				for _, codec := range ps.codecs {
					if err := removeRendition(dir, renditionName(r.n, codec)); err != nil {
						return err
					}
				}
				err = ladder(ctx, url.URL, dir, p, ps, nil)
			}
		}
		if err != nil {
			return snoozeOnShutdown(ctx, err)
		}
		for _, codec := range ps.codecs {
			name := renditionName(r.n, codec)
			path := filepath.Join(dir, name+".mp4")
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			if _, err := parsePlaylist(filepath.Join(dir, name+".m3u8"), st.Size()); err != nil {
				return err
			}
			key, err := c.Encoder.putChunk(ctx, run.ID, ch.Ordinal, name, path)
			if err != nil {
				return snoozeOnShutdown(ctx, err)
			}
			output[name] = key
		}
	}
	if !found {
		return c.cancelRun(ctx, run.ID)
	}
	ch.Output = output
	return c.completeChunk(ctx, job, run, ch)
}

func snoozeOnShutdown(ctx context.Context, err error) error {
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely) {
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
    AND state IN ('available', 'pending', 'retryable', 'scheduled')
    AND args->'ref'->>'tenant_id' IS DISTINCT FROM $2)`,
		(workqueue.VideoChunkArgs{}).Kind(), tenant).Scan(&own, &running, &otherWaiting)
	if err != nil {
		return false, err
	}
	return otherWaiting && own > max(1, running/2), nil
}

func (e *Encoder) putChunk(ctx context.Context, runID string, ordinal int, name, path string) (string, error) {
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
	key := fmt.Sprintf("work/%s/%d/%s-%s.mp4", runID, ordinal, name, hex.EncodeToString(sum))
	if obj, err := e.c.Store.Head(ctx, key); err == nil && obj.Size == size {
		return key, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	opts := media.PutOptions{ContentType: "video/mp4", ChecksumSHA256: sum, CacheControl: blobCacheControl}
	if e.c.Store.Capabilities().ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	_, err = e.c.Store.Put(ctx, key, f, size, opts)
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
			&river.InsertOpts{Queue: workqueue.VideoLightQueue, Priority: priority, MaxAttempts: workqueue.MaxAttempts}); err != nil {
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
