package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

const (
	chunkWindow          = 2
	maxVideoChunksPerRun = 5000
)

// encodeRun is one stage of an upload's encode preset: its rung, encoded in
// bounded chunks, then assembled and published. File is the upload's path,
// Source its blob and Spec the preset and fingerprint (runSpec).
type encodeRun struct {
	ID          string
	Ref         contentref.ContentRef
	File        string
	Source      string
	SourceKey   string
	SourceETag  string
	Spec        string
	Rung        int
	Class       media.VideoJobClass
	State       string
	Probe       probeResult
	Passthrough media.Codec
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

// videoFamily are the presets the video plan produces.
func videoFamily(p *media.Private) bool { return isEncode(p) || p.Subtitles != nil }

// planVideo brings an item's video presets (and frames) up to date: it
// converts subtitles, and probes each source with stale HLS or MP4 outputs
// and records the encode runs; it never downloads a video or encodes one.
func (c WorkerConfig) planVideo(ctx context.Context, args workqueue.VideoPlanArgs) error {
	e := c.Encoder
	item, err := e.ms.Registry().Item(args.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	release, ok, err := pglock.Acquire(ctx, c.Pool, "contentkit:media:video:"+args.Ref.String(), false)
	if err != nil {
		return err
	}
	if !ok {
		return river.JobSnooze(time.Minute)
	}
	defer release()
	if args.Force {
		if err := e.force(ctx, item, args.Preset, videoFamily); err != nil {
			return err
		}
	}
	man, _, err := e.ms.Get(ctx, args.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	for _, f := range man.Files {
		if !f.IsUpload() || f.Gone || f.Blob == "" || f.Fail() != nil {
			continue
		}
		var encode []*media.Private
		for _, p := range item.Kind().PrivateFor(f.Path) {
			if args.Preset != "" && p.Name != args.Preset || !videoFamily(p) || !e.todo(man, f, p) {
				continue
			}
			if p.Subtitles != nil {
				errs = append(errs, e.settle(ctx, item, f, e.subtitle(ctx, item, f, p)))
			} else {
				encode = append(encode, p)
			}
		}
		if len(encode) > 0 {
			err := e.settle(ctx, item, f, c.planUpload(ctx, item, man, f, encode, args.Class))
			errs = append(errs, err)
		}
	}
	if args.Preset == "" {
		errs = append(errs, e.grabFrames(ctx, item))
	}
	return errors.Join(errs...)
}

// force marks preset's outputs (every preset of family when "") stale and
// pending, and clears their uploads' failures, so they are redone.
func (e *Encoder) force(ctx context.Context, item media.Item, preset string, family func(*media.Private) bool) error {
	_, err := e.ms.EditExisting(ctx, item.Ref(), func(m *media.Manifest) error {
		for _, f := range slices.Clone(m.Files) {
			if !f.IsUpload() || f.Gone || f.Blob == "" {
				continue
			}
			for _, p := range item.Kind().PrivateFor(f.Path) {
				if preset != "" && p.Name != preset || !family(p) {
					continue
				}
				for j := range m.Files {
					if o := &m.Files[j]; o.From == f.Path && o.Preset == p.Name {
						o.FP = ""
					}
				}
				m.AddPending(f.Path, p.Name)
				m.Files[m.Find(f.Path)].Failed = nil
			}
		}
		return nil
	})
	if errors.Is(err, media.ErrNotFound) {
		return nil
	}
	return err
}

// planUpload probes upload f and records the runs of its stale encode
// presets. An MP4 at a rung an HLS run of this plan encodes in H.264 is
// remuxed by that run.
func (c WorkerConfig) planUpload(ctx context.Context, item media.Item, man *media.Manifest, f media.File, presets []*media.Private, class media.VideoJobClass) error {
	e := c.Encoder
	key, _ := item.Blob(f.Blob)
	obj, err := e.store.Head(ctx, key)
	if errors.Is(err, media.ErrNotFound) {
		return errStale
	} else if err != nil {
		return err
	}
	// A measured upload was read through once already.
	if f.Dur == 0 {
		if err := e.verify(ctx, item, f.Blob, obj); err != nil {
			return err
		}
	}
	url, err := e.store.PresignGet(ctx, key, 2*time.Hour)
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
	pl, err := newPlan(pr)
	if err != nil {
		return &PermanentError{err}
	}
	if err := e.measured(ctx, item, f, pl, presets); err != nil {
		return err
	}
	k := item.Kind()
	remuxed := map[string]bool{}
	for _, p := range presets {
		if p.HLS == nil {
			continue
		}
		st, err := stages(p, pl)
		if err != nil {
			return &PermanentError{err}
		}
		fp := e.fp(p, f)
		done := stagesDone(man.Outputs(f.Path, p.Name), k.OutputPath(p, f.Path), fp, st, e.c.Codecs)
		if done == len(st) {
			if err := e.refreshSubs(ctx, item, f, p, pl, url.URL); err != nil {
				return err
			}
			continue
		}
		for _, q := range presets {
			if q.MP4 != nil && q.MP4.Profile == p.HLS.Profile && slices.Contains(e.c.Codecs, media.CodecH264) &&
				slices.IndexFunc(st, func(r rung) bool { return r.n == q.MP4.Rung }) >= done {
				remuxed[q.Name] = true
			}
		}
		if err := c.planRuns(ctx, item, f, p, fp, obj, pr, pl, st, done, class); err != nil {
			return err
		}
	}
	for _, p := range presets {
		if p.MP4 == nil || remuxed[p.Name] {
			continue
		}
		st, _ := stages(p, pl)
		if len(st) == 0 {
			if err := e.noOutput(ctx, item, f, p); err != nil {
				return err
			}
			continue
		}
		if err := c.planRuns(ctx, item, f, p, e.fp(p, f), obj, pr, pl, st, 0, class); err != nil {
			return err
		}
	}
	return nil
}

// planRuns sizes a preset's stages into chunks and records them.
func (c WorkerConfig) planRuns(ctx context.Context, item media.Item, f media.File, p *media.Private, fp string, obj media.Object,
	pr probeResult, pl plan, st []rung, done int, class media.VideoJobClass) error {
	codecs := c.Encoder.codecs(p)
	bounds := make([][][2]int64, len(st))
	for i, stage := range st {
		var err error
		if bounds[i], err = chunkBounds(pl, []rung{stage}, len(codecs), c.ChunkTarget); err != nil {
			return &PermanentError{err}
		}
	}
	// The top rung is copied from a source compliant in one of the codecs
	// when it fits one chunk (the chunk checks its packets).
	var passthrough media.Codec
	if src := media.Codec(pl.stream.CodecName); p.HLS != nil && len(st) > 1 && len(bounds[len(bounds)-1]) == 1 &&
		slices.Contains(codecs, src) && (src == media.CodecH264 || src == media.CodecHEVC) {
		codec, ok, why := passthroughCandidate(pl, st[len(st)-1])
		if ok && slices.Contains(codecs, codec) {
			passthrough = codec
		} else {
			c.Logger.DebugContext(ctx, "media/video: no passthrough", "ref", item.Ref().String(), "path", f.Path, "codec", codec, "reason", why)
		}
	}
	key, _ := item.Blob(f.Blob)
	return c.insertRuns(ctx, item.Ref(), f.Path, f.Blob, key, obj.ETag, runSpec(p, fp), class, pr, st, bounds, done, passthrough)
}

// measured records the upload's display size and duration, and marks the
// presets it produces pending (a deploy's recipe change makes outputs stale
// with nothing pending).
func (e *Encoder) measured(ctx context.Context, item media.Item, f media.File, pl plan, presets []*media.Private) error {
	_, err := e.ms.EditExisting(ctx, item.Ref(), func(m *media.Manifest) error {
		if _, err := current(m, f.Path, f.Blob); err != nil {
			return err
		}
		g := &m.Files[m.Find(f.Path)]
		g.W, g.H, g.Dur = pl.width, pl.height, math.Round(pl.duration*1000)/1000
		for _, p := range presets {
			m.AddPending(f.Path, p.Name)
		}
		return nil
	})
	return err
}

// noOutput records that preset p has no output from upload f (an MP4 rung
// above the source).
func (e *Encoder) noOutput(ctx context.Context, item media.Item, f media.File, p *media.Private) error {
	_, err := e.ms.EditExisting(ctx, item.Ref(), func(m *media.Manifest) error {
		if _, err := current(m, f.Path, f.Blob); err != nil {
			return err
		}
		return m.SetOutputs(f.Path, p.Name, nil)
	})
	return err
}

// refreshSubs settles an HLS preset whose renditions are current: its
// source text tracks are extracted again when their cleaning changed, and
// its pending mark cleared.
func (e *Encoder) refreshSubs(ctx context.Context, item media.Item, f media.File, p *media.Private, pl plan, src string) error {
	to, sub := item.Kind().OutputPath(p, f.Path), subsFP(f, p.HLS)
	man, _, err := e.ms.Get(ctx, item.Ref())
	if err != nil {
		return err
	}
	outs := man.Outputs(f.Path, p.Name)
	stale := slices.ContainsFunc(outs, func(o media.File) bool { return trackKind(o) == media.TrackSubs && o.FP != sub })
	if g, ok := man.Get(f.Path); !stale && ok && !slices.Contains(g.Pending, p.Name) {
		return nil
	}
	var subs []media.File
	if stale {
		dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		pl.audio = nil
		if err := ladder(ctx, src, dir, pl, pass{enc: encoding{threads: e.c.Threads}}, nil); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.c.Logger.WarnContext(ctx, "media/video: source subtitles unreadable", "ref", item.Ref().String(), "path", f.Path, "error", err)
			pl.subs = nil
		} else if pl.subs, err = keepSubs(ctx, e.c.Logger, dir, pl.subs); err != nil {
			return err
		}
		if subs, err = e.subsOutputs(ctx, item, dir, to, sub, pl.subs); err != nil {
			return err
		}
	}
	fp := e.fp(p, f)
	return e.publish(ctx, item, subs, func(m *media.Manifest) error {
		g, err := current(m, f.Path, f.Blob)
		if err != nil {
			return err
		}
		next := slices.DeleteFunc(m.Outputs(g.Path, p.Name), func(o media.File) bool { return stale && trackKind(o) == media.TrackSubs })
		next = append(next, subs...)
		orderHLS(next, e.c.Codecs)
		if slices.ContainsFunc(next, func(o media.File) bool { return trackKind(o) != media.TrackSubs && o.FP != fp }) {
			return errStale
		}
		return m.SetOutputs(g.Path, p.Name, next)
	})
}

// subsOutputs stores a pass's kept text tracks (dir/s{i}.vtt) as outputs.
func (e *Encoder) subsOutputs(ctx context.Context, item media.Item, dir, to, fp string, subs []track) ([]media.File, error) {
	var out []media.File
	for i, s := range subs {
		blob, size, err := e.put(ctx, item, filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)), "text/vtt", nil)
		if err != nil {
			return nil, err
		}
		out = append(out, media.File{Path: subsPath(to, s.id), Blob: blob, Type: "text/vtt", Size: size, FP: fp,
			Track: &media.Track{Kind: media.TrackSubs, ID: s.id, Lang: s.lang, Label: s.label, Forced: s.forced}})
	}
	return out, nil
}

func chunkBounds(p plan, rungs []rung, codecs int, target time.Duration) ([][2]int64, error) {
	var pixelFactor float64
	for _, r := range rungs {
		pixelFactor += float64(r.w*r.h) / (1920 * 1080)
	}
	cost := math.Max(0.25, pixelFactor*float64(codecs)*p.fps/30)
	seconds := min(240.0, max(8.0, target.Seconds()/cost))
	step := max(int64(segmentSeconds*1000), int64(seconds/segmentSeconds)*segmentSeconds*1000)
	if p.duration > float64(math.MaxInt64)/1000 {
		return nil, fmt.Errorf("video duration exceeds the supported range")
	}
	end := max(int64(1), int64(math.Ceil(p.duration*1000)))
	count := (end-1)/step + 1
	if count > maxVideoChunksPerRun {
		return nil, fmt.Errorf("video needs %d chunks, above the %d-chunk limit", count, maxVideoChunksPerRun)
	}
	bounds := make([][2]int64, 0, int(count))
	for start := int64(0); start < end; start += step {
		bounds = append(bounds, [2]int64{start, min(start+step, end)})
	}
	return bounds, nil
}

func (c WorkerConfig) insertRuns(ctx context.Context, ref contentref.ContentRef, file, source, key, etag, spec string, class media.VideoJobClass, pr probeResult, stages []rung, bounds [][][2]int64, done int, passthrough media.Codec) error {
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
	previousComplete := true
	for i, stage := range stages {
		var copyCodec media.Codec
		if i == len(stages)-1 {
			copyCodec = passthrough
		}
		priority := workqueue.VideoPlanInsertOpts(class).Priority
		if i > 0 && priority == 1 {
			priority = 2
		}
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO `+c.runTable()+`
  (tenant_id, ref, file_name, source_name, source_key, source_etag, spec, rung, class, probe, passthrough_codec)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (ref, file_name, source_name, source_etag, spec, rung) DO NOTHING
RETURNING id::text`, ref.TenantID, refJSON, file, source, key, etag, spec, stage.n, string(class), probeJSON, string(copyCodec)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `SELECT id::text FROM `+c.runTable()+`
WHERE ref = $1 AND file_name = $2 AND source_name = $3 AND source_etag = $4 AND spec = $5 AND rung = $6`,
				refJSON, file, source, etag, spec, stage.n).Scan(&id); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if i >= done {
			for ordinal, bound := range bounds[i] {
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
  spec, rung, class, state, probe, passthrough_codec FROM `+c.runTable()+` WHERE id = $1`, id).Scan(
		&run.ID, &refJSON, &run.File, &run.Source, &run.SourceKey, &run.SourceETag,
		&run.Spec, &run.Rung, &run.Class, &run.State, &probeJSON, &run.Passthrough)
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
