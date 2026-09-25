package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

func (c WorkerConfig) assemble(ctx context.Context, args workqueue.VideoAssembleArgs, jobID int64) error {
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
	item, err := c.Kinds.Item(run.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	if item.Kind().Video == nil || c.Encoder.Spec(*item.Kind().Video) != run.Spec {
		return c.cancelRun(ctx, run.ID)
	}
	ms, err := media.NewManifests(c.Encoder.c.Store, c.Kinds, media.ManifestOptions{Locker: c.Encoder.c.Locker,
		CacheSize: 1, Sweeps: c.Encoder.c.Sweeps})
	if err != nil {
		return err
	}
	man, _, err := ms.Get(ctx, run.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return c.cancelRun(ctx, run.ID)
	} else if err != nil {
		return err
	}
	i := man.File(run.File)
	if i < 0 || man.Files[i].Source() != run.Source {
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
	stages := slices.Clone(p.rungs)
	slices.Reverse(stages)
	k := slices.IndexFunc(stages, func(r rung) bool { return r.n == run.Rung })
	if k < 0 {
		return c.cancelRun(ctx, run.ID)
	}
	progress := newProgress(ctx, c.report(jobID), c.Encoder.c.ProgressInterval, time.Now, nil)
	fp := progress.file(run.File)
	defer progress.done(run.File)
	fp.set(media.PhaseMuxing)
	if rungPublished(man, run, c.Encoder.c.Codecs) {
		defer progress.item(media.PhaseImages)()
		if err := c.Encoder.images(ctx, ms, item, man); err != nil {
			return err
		}
		return c.finishRun(ctx, run, stages, k)
	}
	chunks, err := c.chunks(ctx, run.ID)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return fmt.Errorf("media/video: run %s has no chunks", run.ID)
	}
	missing, err := c.restoreMissingChunks(ctx, run, chunks, c.Encoder.c.Codecs, k > 0)
	if err != nil {
		return err
	}
	if missing {
		return nil
	}
	dir, err := os.MkdirTemp(c.Encoder.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	url, err := c.Encoder.c.Store.PresignGet(ctx, run.SourceKey, 2*time.Hour)
	if err != nil {
		return err
	}
	var current *media.HLS
	if k > 0 {
		current = man.Files[i].HLS
		if c.Encoder.stagesDone(current, run.Source, run.Spec, stages) != k {
			return fmt.Errorf("media/video: rung %d cannot extend the current manifest", run.Rung)
		}
	}
	if err := c.Encoder.assembleTracks(ctx, item, url.URL, dir, &p, k == 0, current); err != nil {
		return snoozeOnShutdown(ctx, err)
	}
	rungs := []rung{stages[k]}
	hls := &media.HLS{Source: run.Source, Spec: run.Spec, SubsSpec: SubsSpec}
	if k == 0 {
		hls.Pending = rungNames(stages[1:])
	} else {
		*hls = *current
		hls.Video = slices.Clone(current.Video)
		hls.Pending = rungNames(stages[k+1:])
	}
	downloads := make(map[string]media.Download)
	for _, rung := range rungs {
		for _, codec := range c.Encoder.c.Codecs {
			name := renditionName(rung.n, codec)
			if err := c.Encoder.assembleRendition(ctx, dir, name, chunks); err != nil {
				return snoozeOnShutdown(ctx, err)
			}
			path := filepath.Join(dir, name+".mp4")
			w, h, err := renditionFrame(ctx, path)
			if err != nil || w != rung.w || h != rung.h {
				return fmt.Errorf("media/video: assembled %s frame %dx%d, want %dx%d: %w", name, w, h, rung.w, rung.h, err)
			}
			codecs, err := codecString(path)
			if err != nil {
				return err
			}
			if codec == c.Encoder.downloadCodec() {
				if k == 0 {
					if err := c.Encoder.spriteFromRendition(ctx, path, dir, p, rung); err != nil {
						return err
					}
					if err := c.Encoder.storeTracks(ctx, item, dir, p, hls); err != nil {
						return err
					}
				}
				download := filepath.Join(dir, "d"+name+".mp4")
				if err := mux(ctx, dir, name, p, download); err != nil {
					return err
				}
				fp.set(media.PhaseUploading)
				blob, size, err := c.Encoder.put(ctx, item, download, "video/mp4", fp)
				if err != nil {
					return err
				}
				downloads[DownloadKey(run.File, rung.n)] = media.Download{
					Blob: blob, Type: "video/mp4", Size: size, Spec: run.Spec, Inputs: run.Source}
				if err := os.Remove(download); err != nil {
					return err
				}
			}
			blob, pl, err := c.Encoder.stream(ctx, item, dir, name, "video/mp4", fp)
			if err != nil {
				return err
			}
			peak, average := bandwidth(pl.segments)
			hls.Video = append(hls.Video, media.Rendition{Rung: rung.n, Codec: codec, Width: w, Height: h,
				Bandwidth: peak, Average: average, Codecs: codecs, Blob: blob, Segments: pl.segments})
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	var newRenditions []media.Rendition
	if k > 0 {
		newRenditions = slices.Clone(hls.Video[len(current.Video):])
	}
	orderVideo(hls.Video, c.Encoder.c.Codecs)
	// An object can change between the encode and publish. The manifest edit
	// below also fences the logical source name.
	obj, err = c.Encoder.c.Store.Head(ctx, run.SourceKey)
	if errors.Is(err, media.ErrNotFound) || err == nil && obj.ETag != run.SourceETag {
		return c.cancelRun(ctx, run.ID)
	} else if err != nil {
		return err
	}
	fp.set(media.PhasePublishing)
	man, err = ms.Edit(ctx, run.Ref, func(m *media.Manifest) error {
		i := m.File(run.File)
		if i < 0 || m.Files[i].Source() != run.Source {
			return errStale
		}
		if k > 0 {
			old := m.Files[i].HLS
			if old == nil || old.Source != run.Source || old.Spec != run.Spec ||
				!slices.Equal(old.Pending, current.Pending) || len(old.Video) != len(current.Video) ||
				old.SubsSpec != current.SubsSpec || !reflect.DeepEqual(old.Audio, current.Audio) ||
				!reflect.DeepEqual(old.Subs, current.Subs) {
				return errStale
			}
			next := *old
			next.Video = append(slices.Clone(old.Video), newRenditions...)
			orderVideo(next.Video, c.Encoder.c.Codecs)
			next.Pending = hls.Pending
			m.Files[i].HLS = &next
		} else if c.Encoder.stagesDone(m.Files[i].HLS, run.Source, run.Spec, stages) > 0 {
			return errStale
		} else {
			m.Files[i].HLS = hls
		}
		if k == 0 {
			if m.Files[i].Meta == nil {
				m.Files[i].Meta = map[string]any{}
			}
			m.Files[i].Meta["duration"] = p.duration
			m.Files[i].Meta["w"], m.Files[i].Meta["h"] = p.width, p.height
			for key, d := range m.Downloads {
				if name, ok := videoDownload(key, d); ok && name == run.File {
					delete(m.Downloads, key)
				}
			}
		}
		if m.Downloads == nil {
			m.Downloads = make(map[string]media.Download)
		}
		for key, download := range downloads {
			m.Downloads[key] = download
		}
		return nil
	})
	if errors.Is(err, errStale) {
		return c.cancelRun(ctx, run.ID)
	} else if err != nil {
		return err
	}
	defer progress.item(media.PhaseImages)()
	if err := c.Encoder.images(ctx, ms, item, man); err != nil {
		return err
	}
	return c.finishRun(ctx, run, stages, k)
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
			if _, err := c.Encoder.c.Store.Head(ctx, key); errors.Is(err, media.ErrNotFound) {
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

// rungPublished makes an assemble retry safe if its manifest CAS succeeded
// but recording the run's completion did not.
func rungPublished(man *media.Manifest, run encodeRun, codecs []media.Codec) bool {
	i := man.File(run.File)
	if i < 0 {
		return false
	}
	h := man.Files[i].HLS
	if h == nil || h.Source != run.Source || h.Spec != run.Spec || h.Error != "" {
		return false
	}
	for _, codec := range codecs {
		if !slices.ContainsFunc(h.Video, func(v media.Rendition) bool { return v.Rung == run.Rung && v.Codec == codec }) {
			return false
		}
	}
	download, ok := man.Downloads[DownloadKey(run.File, run.Rung)]
	return ok && download.Spec == run.Spec && download.Inputs == run.Source
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

func (e *Encoder) assembleRendition(ctx context.Context, dir, name string, chunks []encodeChunk) error {
	var list strings.Builder
	for _, chunk := range chunks {
		key := chunk.Output[name]
		if key == "" {
			return fmt.Errorf("media/video: chunk %d lacks %s", chunk.Ordinal, name)
		}
		request, err := e.c.Store.PresignGet(ctx, key, 2*time.Hour)
		if err != nil {
			return err
		}
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(request.URL, "'", "'\\''"))
	}
	listPath := filepath.Join(dir, name+"-list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o600); err != nil {
		return err
	}
	defer os.Remove(listPath)
	args := []string{"-v", "error", "-nostdin", "-y", "-protocol_whitelist", "file,http,https,tcp,tls,crypto", "-f", "concat", "-safe", "0", "-i", listPath,
		"-map", "0:v:0", "-an", "-sn", "-c", "copy"}
	args = append(args, hlsArgs(filepath.Join(dir, name+".mp4"), filepath.Join(dir, name+".m3u8"))...)
	_, err := command(ctx, "ffmpeg", args...)
	return err
}

func (e *Encoder) fetchWork(ctx context.Context, key, path string) error {
	object, meta, err := e.c.Store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return err
	}
	defer object.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, object)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n != meta.Size {
		err = fmt.Errorf("media/video: work object %s: read %d of %d bytes", key, n, meta.Size)
	}
	return err
}

func (e *Encoder) assembleTracks(ctx context.Context, item media.Item, src, dir string, p *plan, first bool, current *media.HLS) error {
	if !first {
		for i, audio := range current.Audio {
			key, err := item.Private(audio.Blob)
			if err != nil {
				return err
			}
			if err := e.fetchWork(ctx, key, filepath.Join(dir, fmt.Sprintf("a%d.mp4", i))); err != nil {
				return err
			}
		}
		for i, sub := range current.Subs {
			key, err := item.Private(sub.Blob)
			if err != nil {
				return err
			}
			if err := e.fetchWork(ctx, key, filepath.Join(dir, fmt.Sprintf("s%d.vtt", i))); err != nil {
				return err
			}
		}
		tracks := make([]track, 0, len(current.Subs))
		for _, sub := range current.Subs {
			for _, candidate := range p.subs {
				if candidate.id == sub.ID {
					tracks = append(tracks, candidate)
					break
				}
			}
		}
		p.subs = tracks
		return nil
	}
	if len(p.audio)+len(p.subs) > 0 {
		ps := pass{enc: encoding{threads: e.c.Threads}}
		if err := ladder(ctx, src, dir, *p, ps, nil); err != nil {
			return err
		}
	}
	var err error
	p.subs, err = keepSubs(ctx, e.c.Logger, dir, p.subs)
	return err
}

// spriteFromRendition samples the already-assembled first rung, so the light
// job never decodes a whole 4K source merely to produce thumbnails.
func (e *Encoder) spriteFromRendition(ctx context.Context, path, dir string, p plan, rung rung) error {
	p.video, p.audio, p.subs = 0, nil, nil
	ps := pass{rung: rung, sprite: true, noTracks: true,
		enc: encoding{threads: e.c.Threads}}
	return ladder(ctx, path, dir, p, ps, nil)
}

func (e *Encoder) storeTracks(ctx context.Context, item media.Item, dir string, p plan, hls *media.HLS) error {
	for i, track := range p.audio {
		blob, pl, err := e.stream(ctx, item, dir, fmt.Sprintf("a%d", i), "audio/mp4", nil)
		if err != nil {
			return err
		}
		peak, _ := bandwidth(pl.segments)
		hls.Audio = append(hls.Audio, media.AudioTrack{ID: track.id, Lang: track.lang, Label: track.label,
			Default: track.def, Bandwidth: peak, Codecs: "mp4a.40.2", Blob: blob, Segments: pl.segments})
	}
	for i, sub := range p.subs {
		blob, _, err := e.put(ctx, item, filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)), "text/vtt", nil)
		if err != nil {
			return err
		}
		hls.Subs = append(hls.Subs, media.Subtitle{ID: sub.id, Lang: sub.lang, Label: sub.label,
			Forced: sub.forced, Blob: blob})
	}
	if _, err := os.Stat(filepath.Join(dir, "sprite.jpg")); err == nil {
		blob, _, err := e.put(ctx, item, filepath.Join(dir, "sprite.jpg"), "image/jpeg", nil)
		if err != nil {
			return err
		}
		hls.Sprite = &media.Sprite{Blob: blob, Cols: spriteCols, Rows: spriteRows, Width: p.tileW,
			Height: p.tileH, Interval: p.duration / (spriteCols * spriteRows)}
	}
	return nil
}
