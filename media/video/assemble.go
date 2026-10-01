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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// assemble joins a run's chunks and publishes its stage: an HLS rung (the
// first stage also the source's audio and text tracks and the sprite, and
// the MP4s at its rung remuxed from H.264), or an MP4. Then it grabs the
// frames the new renditions serve and releases the next stage.
func (c WorkerConfig) assemble(ctx context.Context, args workqueue.VideoAssembleArgs, jobID int64) error {
	e := c.Encoder
	run, err := c.loadRun(ctx, args.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if !run.Ref.Equal(args.Ref) || run.State == "cancelled" || run.State == "complete" {
		return nil
	}
	if run.State != "assembling" {
		return river.JobSnooze(30 * time.Second)
	}
	w, ok, err := c.current(ctx, run)
	if err != nil {
		return err
	}
	k := slices.IndexFunc(w.stages, func(r rung) bool { return r.n == run.Rung })
	if !ok || k < 0 {
		return c.cancelRun(ctx, run.ID)
	}
	progress := newProgress(ctx, c.report(jobID), e.c.ProgressInterval, time.Now, nil)
	fp := progress.file(run.File)
	defer progress.done(run.File)
	fp.set(media.PhaseMuxing)
	if !w.published(e, k) {
		chunks, err := c.chunks(ctx, run.ID)
		if err != nil {
			return err
		}
		if len(chunks) == 0 {
			return fmt.Errorf("media/video: run %s has no chunks", run.ID)
		}
		missing, err := c.restoreMissingChunks(ctx, run, chunks, e.codecs(w.p), k > 0)
		if err != nil || missing {
			return err
		}
		dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		url, err := e.store.PresignGet(ctx, run.SourceKey, 2*time.Hour)
		if err != nil {
			return err
		}
		if w.p.HLS != nil {
			err = e.assembleHLS(ctx, w, k, chunks, dir, url.URL, fp)
		} else {
			err = e.assembleMP4(ctx, w, chunks, dir, url.URL, fp)
		}
		if errors.Is(err, errStale) {
			return c.cancelRun(ctx, run.ID)
		} else if err != nil {
			return snoozeOnShutdown(ctx, err)
		}
	}
	done := progress.item(media.PhaseImages)
	err = e.grabFrames(ctx, w.item)
	done()
	if err != nil {
		return err
	}
	return c.finishRun(ctx, run, w.stages, k)
}

// published reports a stage already recorded (an assemble retried after
// its manifest edit).
func (w runWork) published(e *Encoder, k int) bool {
	outs := w.man.Outputs(w.f.Path, w.p.Name)
	if w.p.MP4 != nil {
		return len(outs) == 1 && outs[0].FP == w.fp
	}
	return stagesDone(outs, w.to(), w.fp, w.stages, e.c.Codecs) > k
}

func (e *Encoder) assembleHLS(ctx context.Context, w runWork, k int, chunks []encodeChunk, dir, src string, fp *fileProgress) error {
	to, r, pl := w.to(), w.stages[k], w.pl
	var mp4s []*media.Private // MP4 presets remuxed from this rung's H.264
	if slices.Contains(e.c.Codecs, media.CodecH264) {
		for _, q := range w.item.Kind().PrivateFor(w.f.Path) {
			if q.MP4 != nil && q.MP4.Rung == r.n && q.MP4.Profile == w.p.HLS.Profile && e.todo(w.man, w.f, q) {
				mp4s = append(mp4s, q)
			}
		}
	}
	// The first stage encodes the source's audio and text tracks; a later
	// one muxing an MP4 fetches the published default audio track.
	audio := ""
	if k == 0 {
		if len(pl.audio)+len(pl.subs) > 0 {
			if err := ladder(ctx, src, dir, pl, pass{enc: encoding{threads: e.c.Threads}}, nil); err != nil {
				return err
			}
		}
		var err error
		if pl.subs, err = keepSubs(ctx, e.c.Logger, dir, pl.subs); err != nil {
			return err
		}
		if _, i, ok := pl.defaultAudio(); ok {
			audio = filepath.Join(dir, fmt.Sprintf("a%d.mp4", i))
		}
	} else if len(mp4s) > 0 {
		for _, o := range w.man.Outputs(w.f.Path, w.p.Name) {
			if o.Track != nil && o.Track.Kind == media.TrackAudio && o.Track.Default {
				audio = filepath.Join(dir, "default-audio.mp4")
				if err := e.fetch(ctx, w.item, o.Blob, audio, fp); err != nil {
					return err
				}
			}
		}
	}
	var outs, mp4Files []media.File
	spriteCodec := e.c.Codecs[0]
	if slices.Contains(e.c.Codecs, media.CodecH264) {
		spriteCodec = media.CodecH264
	}
	for _, codec := range e.c.Codecs {
		name := renditionName(r.n, codec)
		path := filepath.Join(dir, name+".mp4")
		width, height, codecs, err := e.assembleRendition(ctx, dir, name, r, chunks)
		if err != nil {
			return err
		}
		if k == 0 && codec == spriteCodec {
			if err := e.spriteFromRendition(ctx, path, dir, pl, r); err != nil {
				return err
			}
		}
		if codec == media.CodecH264 && len(mp4s) > 0 {
			files, err := e.muxMP4(ctx, w, mp4s, path, audio, width, height, fp)
			if err != nil {
				return err
			}
			mp4Files = append(mp4Files, files...)
		}
		fp.set(media.PhaseUploading)
		o, err := e.stream(ctx, w.item, dir, name, renditionPath(to, r.n, codec), "video/mp4", fp)
		if err != nil {
			return err
		}
		o.W, o.H, o.FP = width, height, w.fp
		o.Track.Kind, o.Track.Codec, o.Track.Codecs = media.TrackVideo, string(codec), codecs
		outs = append(outs, o)
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if k == 0 {
		for i, a := range pl.audio {
			o, err := e.stream(ctx, w.item, dir, fmt.Sprintf("a%d", i), audioPath(to, a.id), "audio/mp4", fp)
			if err != nil {
				return err
			}
			o.FP, o.Track = w.fp, &media.Track{Kind: media.TrackAudio, ID: a.id, Lang: a.lang, Label: a.label, Default: a.def,
				Bandwidth: o.Track.Bandwidth, Codecs: "mp4a.40.2", Index: o.Track.Index}
			outs = append(outs, o)
		}
		subs, err := e.subsOutputs(ctx, w.item, dir, to, subsFP(w.f, w.p.HLS), pl.subs)
		if err != nil {
			return err
		}
		sprite, err := e.sprite(ctx, w.item, dir, to, pl, fp)
		if err != nil {
			return err
		}
		sprite.FP = w.fp
		outs = append(append(outs, subs...), sprite)
	}
	fp.set(media.PhasePublishing)
	return e.publish(ctx, w.item, append(outs, mp4Files...), func(m *media.Manifest) error {
		g, err := current(m, w.f.Path, w.f.Blob)
		if err != nil || e.fp(w.p, g) != w.fp {
			return errStale
		}
		cur := m.Outputs(g.Path, w.p.Name)
		switch done := stagesDone(cur, to, w.fp, w.stages, e.c.Codecs); {
		case done < k:
			return errStale
		case done == k:
			next := outs
			if k > 0 {
				next = append(slices.Clone(cur), outs...)
			}
			orderHLS(next, e.c.Codecs)
			if err := m.SetOutputs(g.Path, w.p.Name, next); err != nil {
				return err
			}
			if k+1 < len(w.stages) {
				m.AddPending(g.Path, w.p.Name)
			}
		}
		return setMP4s(m, g.Path, mp4Files)
	})
}

// assembleMP4 publishes an MP4 run: its H.264 rung muxed with the source's
// default audio track.
func (e *Encoder) assembleMP4(ctx context.Context, w runWork, chunks []encodeChunk, dir, src string, fp *fileProgress) error {
	r, pl := w.stages[0], w.pl
	name := renditionName(r.n, media.CodecH264)
	width, height, _, err := e.assembleRendition(ctx, dir, name, r, chunks)
	if err != nil {
		return err
	}
	audio := ""
	if a, _, ok := pl.defaultAudio(); ok {
		pl.audio, pl.subs = []track{a}, nil
		if err := ladder(ctx, src, dir, pl, pass{enc: encoding{threads: e.c.Threads}}, nil); err != nil {
			return err
		}
		audio = filepath.Join(dir, "a0.mp4")
	}
	files, err := e.muxMP4(ctx, w, []*media.Private{w.p}, filepath.Join(dir, name+".mp4"), audio, width, height, fp)
	if err != nil {
		return err
	}
	fp.set(media.PhasePublishing)
	return e.publish(ctx, w.item, files, func(m *media.Manifest) error {
		g, err := current(m, w.f.Path, w.f.Blob)
		if err != nil || e.fp(w.p, g) != w.fp {
			return errStale
		}
		return setMP4s(m, g.Path, files)
	})
}

// muxMP4 muxes a rendition and an audio track into the MP4 presets' file.
func (e *Encoder) muxMP4(ctx context.Context, w runWork, presets []*media.Private, video, audio string, width, height int, fp *fileProgress) ([]media.File, error) {
	out := strings.TrimSuffix(video, ".mp4") + "-muxed.mp4"
	if err := mux(ctx, video, audio, out); err != nil {
		return nil, err
	}
	defer os.Remove(out)
	fp.set(media.PhaseUploading)
	blob, size, err := e.put(ctx, w.item, out, "video/mp4", fp)
	if err != nil {
		return nil, err
	}
	var files []media.File
	for _, q := range presets {
		files = append(files, media.File{Path: w.item.Kind().OutputPath(q, w.f.Path), Blob: blob, Type: "video/mp4", Size: size,
			W: width, H: height, Dur: math.Round(w.pl.duration*1000) / 1000, Preset: q.Name, FP: e.fp(q, w.f)})
	}
	return files, nil
}

// setMP4s records MP4 preset outputs (File.Preset names each one's preset).
func setMP4s(m *media.Manifest, from string, files []media.File) error {
	for _, f := range files {
		if err := m.SetOutputs(from, f.Preset, []media.File{f}); err != nil {
			return err
		}
	}
	return nil
}

// sprite stores a first stage's seek sprite and its grid.
func (e *Encoder) sprite(ctx context.Context, item media.Item, dir, to string, pl plan, fp *fileProgress) (media.File, error) {
	blob, size, err := e.put(ctx, item, filepath.Join(dir, "sprite.jpg"), "image/jpeg", fp)
	if err != nil {
		return media.File{}, err
	}
	index, err := e.putJSON(ctx, item, media.TrackIndex{Sprite: &media.Sprite{Cols: spriteCols, Rows: spriteRows,
		W: pl.tileW, H: pl.tileH, Interval: pl.duration / (spriteCols * spriteRows)}})
	if err != nil {
		return media.File{}, err
	}
	return media.File{Path: spritePath(to), Blob: blob, Type: "image/jpeg", Size: size, W: spriteCols * pl.tileW, H: spriteRows * pl.tileH,
		Track: &media.Track{Kind: media.TrackSprite, Index: index}}, nil
}

// restoreMissingChunks reopens an assembling run if temporary work objects
// expired before assembly. The replacement chunks will queue a new assemble.
func (c WorkerConfig) restoreMissingChunks(ctx context.Context, run encodeRun, chunks []encodeChunk, codecs []media.Codec, laterRung bool) (bool, error) {
	var missing []int
	for _, ch := range chunks {
		for _, codec := range codecs {
			key := ch.Output[renditionName(run.Rung, codec)]
			if key == "" {
				missing = append(missing, ch.Ordinal)
				break
			}
			if _, err := c.Encoder.store.Head(ctx, key); errors.Is(err, media.ErrNotFound) {
				missing = append(missing, ch.Ordinal)
				break
			} else if err != nil {
				return false, err
			}
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM `+c.runTable()+` WHERE id = $1 FOR UPDATE`, run.ID).Scan(&state); err != nil {
		return false, err
	}
	if state != "assembling" {
		return true, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE `+c.chunkTable()+`
SET state = 'planned', job_id = NULL, output = NULL, finished_at = NULL
WHERE run_id = $1 AND ordinal = ANY($2)`, run.ID, missing); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'encoding', completed = completed - $2, updated_at = now() WHERE id = $1`, run.ID, len(missing)); err != nil {
		return false, err
	}
	priority := workqueue.VideoPlanInsertOpts(run.Class).Priority
	if laterRung && priority == 1 {
		priority = 2
	}
	if err := c.releaseWindow(ctx, tx, run.ID, run.Ref, priority); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (c WorkerConfig) finishRun(ctx context.Context, run encodeRun, stages []rung, k int) error {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM `+c.runTable()+` WHERE id = $1 FOR UPDATE`, run.ID).Scan(&state); err != nil {
		return err
	}
	if state == "complete" || state == "cancelled" {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE `+c.runTable()+`
SET state = 'complete', updated_at = now() WHERE id = $1`, run.ID); err != nil {
		return err
	}
	if k+1 < len(stages) {
		refJSON, err := json.Marshal(run.Ref)
		if err != nil {
			return err
		}
		var nextID string
		err = tx.QueryRow(ctx, `SELECT id::text FROM `+c.runTable()+`
WHERE ref = $1 AND file_name = $2 AND source_name = $3 AND source_etag = $4 AND spec = $5 AND rung = $6`,
			refJSON, run.File, run.Source, run.SourceETag, run.Spec, stages[k+1].n).Scan(&nextID)
		if err != nil {
			return err
		}
		priority := workqueue.VideoPlanInsertOpts(run.Class).Priority
		if priority == 1 {
			priority = 2
		}
		if err := c.releaseWindow(ctx, tx, nextID, run.Ref, priority); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// assembleRendition joins rendition name's chunks into dir/name.mp4 and
// checks its frame is rung r's; it returns the frame and RFC 6381 codecs.
func (e *Encoder) assembleRendition(ctx context.Context, dir, name string, r rung, chunks []encodeChunk) (int, int, string, error) {
	var list strings.Builder
	for _, chunk := range chunks {
		key := chunk.Output[name]
		if key == "" {
			return 0, 0, "", fmt.Errorf("media/video: chunk %d lacks %s", chunk.Ordinal, name)
		}
		request, err := e.store.PresignGet(ctx, key, 2*time.Hour)
		if err != nil {
			return 0, 0, "", err
		}
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(request.URL, "'", "'\\''"))
	}
	listPath := filepath.Join(dir, name+"-list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o600); err != nil {
		return 0, 0, "", err
	}
	defer os.Remove(listPath)
	args := []string{"-v", "error", "-nostdin", "-y", "-protocol_whitelist", "file,http,https,tcp,tls,crypto", "-f", "concat", "-safe", "0", "-i", listPath,
		"-map", "0:v:0", "-an", "-sn", "-c", "copy"}
	args = append(args, hlsArgs(filepath.Join(dir, name+".mp4"), filepath.Join(dir, name+".m3u8"))...)
	if _, err := command(ctx, "ffmpeg", args...); err != nil {
		return 0, 0, "", err
	}
	path := filepath.Join(dir, name+".mp4")
	w, h, err := renditionFrame(ctx, path)
	if err != nil || w != r.w || h != r.h {
		return 0, 0, "", fmt.Errorf("media/video: assembled %s frame %dx%d, want %dx%d: %w", name, w, h, r.w, r.h, err)
	}
	codecs, err := codecString(path)
	return w, h, codecs, err
}

// spriteFromRendition samples the already-assembled first rung, so the light
// job never decodes a whole 4K source merely to produce thumbnails.
func (e *Encoder) spriteFromRendition(ctx context.Context, path, dir string, p plan, rung rung) error {
	p.video, p.audio, p.subs = 0, nil, nil
	ps := pass{rung: rung, sprite: true, noTracks: true,
		enc: encoding{threads: e.c.Threads}}
	return ladder(ctx, path, dir, p, ps, nil)
}
