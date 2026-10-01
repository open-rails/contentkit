package video

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/open-rails/contentkit/media"
)

// grabFrames grabs the frames of uploads declaring Upload.Frames that have
// none from their video's current blob: at Frame.T, or the first detailed
// frame when Frame.Auto. Each is recorded as the upload's PNG blob, at the
// video's display size (the space of its edit), and handed to the image job.
func (e *Encoder) grabFrames(ctx context.Context, item media.Item) error {
	man, _, err := e.ms.Get(ctx, item.Ref())
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	grabbed := false
	for _, f := range man.Files {
		if !f.IsUpload() || f.Frame == nil || f.Fail() != nil {
			continue
		}
		v, ok := item.Kind().FramesVideo(man, f.Path)
		if !ok || v.Blob == "" || f.Blob != "" && f.Frame.Of == v.Blob {
			continue
		}
		if fail := v.Fail(); fail != nil || v.Gone {
			cause := errors.New("its video cannot be read")
			if fail != nil {
				cause = fmt.Errorf("its video failed: %s", fail.Message)
			}
			errs = append(errs, e.failed(ctx, item, f.Path, f.Blob, cause, func(m *media.Manifest) { m.SetFailed(f.Path, cause) }))
			continue
		}
		err := e.grab(ctx, item, man, f, v)
		var perm *PermanentError
		switch {
		case errors.Is(err, errStale):
		case errors.As(err, &perm):
			errs = append(errs, e.fail(ctx, item, f.Path, f.Blob, perm.Err))
		case err != nil:
			errs = append(errs, fmt.Errorf("media/video: frame %s %q: %w", item.Ref(), f.Path, err))
		default:
			grabbed = true
		}
	}
	if grabbed {
		if e.c.Queue == nil {
			e.c.Logger.WarnContext(ctx, "media/video: no Config.Queue; grabbed frames are not rendered", "ref", item.Ref().String())
		} else if err := e.c.Queue.Enqueue(ctx, media.ProcessJob{Ref: item.Ref()}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// grab grabs frame upload f from video v.
func (e *Encoder) grab(ctx context.Context, item media.Item, man *media.Manifest, f, v media.File) error {
	src, err := e.frameSource(ctx, item, man, v)
	if err != nil {
		return err
	}
	if v.Dur == 0 {
		if v, err = e.measure(ctx, item, v, src.url); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	t := f.Frame.T
	if f.Frame.Auto {
		if t, err = e.autoFrame(ctx, item, src, v.Dur, dir); err != nil {
			return err
		}
	}
	t = clampTime(t, v.Dur)
	in, err := src.input(ctx, e.store, t, filepath.Join(dir, "frame.mp4"))
	if err != nil {
		return err
	}
	w, h := frameSize(v.W, v.H)
	out := filepath.Join(dir, "frame.png")
	if err := grabFrame(ctx, in, w, h, out, e.c.Threads); err != nil {
		return err
	}
	body, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	blob, size, err := e.putBlob(ctx, item, bytes.NewReader(body), int64(len(body)), sum[:], "image/png", nil)
	if err != nil {
		return err
	}
	grabbed := media.File{Blob: blob}
	err = e.publish(ctx, item, []media.File{grabbed}, func(m *media.Manifest) error {
		if _, err := current(m, v.Path, v.Blob); err != nil {
			return err
		}
		i := m.Find(f.Path)
		if i < 0 {
			return errStale
		}
		g := &m.Files[i]
		if g.Frame == nil || g.Frame.Auto != f.Frame.Auto || !g.Frame.Auto && g.Frame.T != f.Frame.T || g.Blob != f.Blob {
			return errStale
		}
		g.Blob, g.Type, g.Size, g.W, g.H, g.Failed = blob, "image/png", size, w, h, nil
		g.Frame = &media.Frame{T: t, Auto: g.Frame.Auto, Of: v.Blob}
		if g.Edit.Check(w, h) != nil { // made for another shape: centred instead
			g.Edit = nil
		}
		return nil
	})
	if err == nil {
		e.c.Logger.InfoContext(ctx, "media/video: frame", "ref", item.Ref().String(), "path", f.Path, "time", t, "auto", f.Frame.Auto)
	}
	return err
}

// frameSize is a grabbed frame's size: the video's display size, within
// the rungs' frame caps.
func frameSize(w, h int) (int, int) {
	if max(w, h) <= maxSide && w*h <= maxArea {
		return w, h
	}
	return frame(w, h, min(w, h))
}

// measure probes a video upload that no plan measured (its kind encodes
// nothing from it) and records its display size and duration.
func (e *Encoder) measure(ctx context.Context, item media.Item, v media.File, url string) (media.File, error) {
	if url == "" {
		var err error
		if url, err = e.sourceURL(ctx, item, v); err != nil {
			return v, err
		}
	}
	pr, err := probeRemote(ctx, url)
	if err != nil {
		if ctx.Err() != nil {
			return v, ctx.Err()
		}
		return v, &PermanentError{err}
	}
	pl, err := newPlan(pr)
	if err != nil {
		return v, &PermanentError{err}
	}
	if err := e.measured(ctx, item, v, pl, nil); err != nil {
		return v, err
	}
	v.W, v.H, v.Dur = pl.width, pl.height, pl.duration
	return v, nil
}

func (e *Encoder) sourceURL(ctx context.Context, item media.Item, v media.File) (string, error) {
	key, err := item.Blob(v.Blob)
	if err != nil {
		return "", err
	}
	req, err := e.store.PresignGet(ctx, key, time.Hour)
	return req.URL, err
}

// frameSource is where frames are cut: the widest current rendition of the
// video (H.264 when there is one, any ffmpeg decodes it), read by range, or
// else the source itself.
type frameSource struct {
	key  string
	segs []media.Segment
	url  string
}

func (e *Encoder) frameSource(ctx context.Context, item media.Item, man *media.Manifest, v media.File) (frameSource, error) {
	var best *media.File
	for _, p := range item.Kind().PrivateFor(v.Path) {
		if p.HLS == nil {
			continue
		}
		fp := e.fp(p, v)
		for _, o := range man.Outputs(v.Path, p.Name) {
			if o.FP == fp && o.Track != nil && o.Track.Kind == media.TrackVideo && (best == nil ||
				cmp.Or(o.W-best.W, boolInt(o.Track.Codec == string(media.CodecH264))-boolInt(best.Track.Codec == string(media.CodecH264))) > 0) {
				best = &o
			}
		}
	}
	if best == nil {
		url, err := e.sourceURL(ctx, item, v)
		return frameSource{url: url}, err
	}
	idx, err := e.readIndex(ctx, item, best.Track.Index)
	if err != nil {
		return frameSource{}, err
	}
	key, _ := item.Blob(best.Blob)
	return frameSource{key: key, segs: idx.Segments}, nil
}

// input is an ffmpeg input at t: a local snippet of the rendition's
// segments around t (written to path), or the source.
func (s frameSource) input(ctx context.Context, store media.Store, t float64, path string) (frameInput, error) {
	if s.url != "" {
		return frameInput{path: s.url, opts: remoteInputOptions(sourceDemuxers), offset: t}, nil
	}
	start, err := snippet(ctx, store, s.key, s.segs, t, t, path)
	return frameInput{path: path, opts: ownMP4, offset: t - start}, err
}

// autoFrame picks the earliest sampled frame with detail, else the most detailed.
func (e *Encoder) autoFrame(ctx context.Context, item media.Item, src frameSource, d float64, dir string) (float64, error) {
	best, bestT := -1.0, 0.0
	for i, frac := range autoPosterAt {
		t := clampTime(d*frac, d)
		in, err := src.input(ctx, e.store, t, filepath.Join(dir, fmt.Sprintf("probe%d.mp4", i)))
		if err != nil {
			return 0, err
		}
		v, err := detail(ctx, in, e.c.Threads)
		if err != nil {
			return 0, err
		}
		if v >= minDetail {
			return t, nil
		}
		if v > best {
			best, bestT = v, t
		}
	}
	return bestT, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
