package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// images brings the item's poster frame and hover preview up to their
// selections from this manifest's video file. Selections of another version
// are left to that version's job; uploaded posters to the image job.
func (e *Encoder) images(ctx context.Context, ms *media.Manifests, item media.Item, man *media.Manifest) error {
	return errors.Join(e.poster(ctx, ms, item, man), e.preview(ctx, ms, item, man))
}

func duration(f media.File) float64 { d, _ := f.Meta["duration"].(float64); return d }

func (e *Encoder) poster(ctx context.Context, ms *media.Manifests, item media.Item, man *media.Manifest) error {
	ref := item.Ref()
	for range 4 {
		rec, err := ms.Slot(ctx, ref.Content(), media.PosterSlot)
		if errors.Is(err, media.ErrNotFound) {
			rec = &media.SlotRecord{}
		} else if err != nil {
			return err
		}
		var sel media.PosterFrame
		switch {
		case rec.Frame != nil:
			sel = *rec.Frame
		case rec.Original != "":
			return nil // uploaded
		default:
			sel = media.PosterFrame{Auto: true, Version: ref.Version()}
		}
		if sel.Version != ref.Version() {
			return nil
		}
		f, ok := media.VideoFile(man, sel.File)
		if !ok && sel.Auto {
			f, ok = media.VideoFile(man, "")
		}
		if !ok || !media.Encoded(f) {
			return nil
		}
		if sel.Source == f.Source() && rec.Original != "" {
			// Grabbed; the image job encodes it (again, if its hand-off was lost).
			if rec.Result == nil || rec.Result.Of != rec.Fingerprint(media.VideoPoster) {
				return e.encodePoster(ctx, item)
			}
			return nil
		}
		err = e.grabPoster(ctx, ms, item, rec, sel, f)
		if !errors.Is(err, media.ErrSuperseded) {
			return err
		}
	}
	return fmt.Errorf("media/video: poster of %s kept changing", ref)
}

func (e *Encoder) encodePoster(ctx context.Context, item media.Item) error {
	if e.c.Slots == nil {
		e.c.Logger.WarnContext(ctx, "media/video: no Config.Slots; poster frame not handed to the image job", "ref", item.Ref().String())
		return nil
	}
	return e.c.Slots.Enqueue(ctx, media.ProcessJob{Ref: item.Ref().Content(), Slot: media.PosterSlot})
}

// grabPoster grabs the selected (or automatic) frame from the widest
// rendition into originals/poster, records it and hands it to the image job.
func (e *Encoder) grabPoster(ctx context.Context, ms *media.Manifests, item media.Item, rec *media.SlotRecord, sel media.PosterFrame, f media.File) error {
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	r, _ := media.FrameRendition(f)
	size := media.PosterFrameSize(r.Width, r.Height)
	d := duration(f)
	t := sel.Time
	if sel.Auto {
		if t, err = e.autoFrame(ctx, item, r, d, dir); err != nil {
			return err
		}
	}
	t = clampTime(t, d)
	clip := filepath.Join(dir, "frame.mp4")
	start, err := snippet(ctx, e.c.Store, item, r, t, t, clip)
	if err != nil {
		return err
	}
	frame := filepath.Join(dir, "frame.png")
	if err := grabFrame(ctx, clip, t-start, size.W, size.H, frame, e.c.Threads); err != nil {
		return err
	}
	body, err := os.ReadFile(frame)
	if err != nil {
		return err
	}
	// An edit made for a replaced source of another shape falls back to centred.
	edit := rec.Edit
	if _, err := media.VideoPoster.Resolve(edit, size.W, size.H); err != nil {
		edit = nil
	}

	key, _ := item.SlotOriginal(media.PosterSlot)
	opts := media.PutOptions{ContentType: "image/png"}
	if e.c.Store.Capabilities().ConditionalPut {
		prev, err := e.c.Store.Head(ctx, key)
		switch {
		case errors.Is(err, media.ErrNotFound):
			opts.IfNoneMatch = "*"
		case err != nil:
			return err
		default:
			opts.IfMatch = prev.ETag
		}
	}
	obj, err := e.c.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), opts)
	if errors.Is(err, media.ErrPreconditionFailed) {
		return media.ErrSuperseded // an upload landed meanwhile
	} else if err != nil {
		return err
	}
	if err := ms.UpdateSlot(ctx, item.Ref().Content(), media.PosterSlot, func(cur *media.SlotRecord) error {
		unchanged := cur.Frame == nil && rec.Frame == nil && cur.Original == "" ||
			cur.Frame != nil && rec.Frame != nil && cur.Frame.Same(sel) && cur.Edit.Hash() == rec.Edit.Hash()
		if !unchanged {
			return media.ErrSuperseded
		}
		cur.Original, cur.Edit = obj.ETag, edit
		cur.Frame = &media.PosterFrame{Version: sel.Version, File: f.Name, Time: t, Auto: sel.Auto, Source: f.Source()}
		return nil
	}); err != nil {
		return err
	}
	e.c.Logger.InfoContext(ctx, "media/video: poster frame", "ref", item.Ref().String(), "file", f.Name, "time", t, "auto", sel.Auto)
	return e.encodePoster(ctx, item)
}

// autoFrame picks the earliest sampled frame with detail, else the most detailed.
func (e *Encoder) autoFrame(ctx context.Context, item media.Item, r media.Rendition, d float64, dir string) (float64, error) {
	best, bestT := -1.0, 0.0
	for i, frac := range autoPosterAt {
		t := clampTime(d*frac, d)
		clip := filepath.Join(dir, fmt.Sprintf("probe%d.mp4", i))
		start, err := snippet(ctx, e.c.Store, item, r, t, t, clip)
		if err != nil {
			return 0, err
		}
		v, err := detail(ctx, clip, t-start, e.c.Threads)
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

func (e *Encoder) preview(ctx context.Context, ms *media.Manifests, item media.Item, man *media.Manifest) error {
	ref := item.Ref()
	for range 4 {
		rec, err := ms.HoverPreview(ctx, ref)
		if errors.Is(err, media.ErrNotFound) {
			rec = nil
		} else if err != nil {
			return err
		}
		sel := media.HoverPreviewRecord{Auto: true, Version: ref.Version()}
		if rec != nil {
			sel = *rec
			sel.Result = nil
		}
		if sel.Version != ref.Version() {
			return nil
		}
		f, ok := media.VideoFile(man, sel.File)
		if !ok && sel.Auto {
			f, ok = media.VideoFile(man, "")
		}
		if !ok || !media.Encoded(f) {
			return nil
		}
		if sel.Auto {
			sel.File = f.Name
			sel.Start, sel.Duration = media.AutoHoverPreview(duration(f))
		}
		if rec != nil && rec.Key() == sel.Key() && rec.Result != nil && rec.Result.Of == sel.Key() &&
			rec.Result.Source == f.Source() && rec.Result.Recipe == PreviewRecipe {
			return nil
		}
		err = e.renderHoverPreview(ctx, ms, item, rec, sel, f)
		if !errors.Is(err, media.ErrSuperseded) {
			return err
		}
	}
	return fmt.Errorf("media/video: hover preview of %s kept changing", ref)
}

func (e *Encoder) renderHoverPreview(ctx context.Context, ms *media.Manifests, item media.Item, rec *media.HoverPreviewRecord, sel media.HoverPreviewRecord, f media.File) error {
	ref := item.Ref()
	if rec == nil || rec.Key() != sel.Key() {
		if err := ms.UpdateHoverPreview(ctx, ref, func(cur *media.HoverPreviewRecord) (*media.HoverPreviewRecord, error) {
			if (cur == nil) != (rec == nil) || cur != nil && cur.Key() != rec.Key() {
				return nil, media.ErrSuperseded
			}
			next := sel
			if cur != nil {
				next.Result = cur.Result
			}
			return &next, nil
		}); err != nil {
			return err
		}
	}

	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	top, _ := media.FrameRendition(f)
	sizes := media.HoverPreviewSizes(top.Width, top.Height)
	r := narrowest(f, sizes[len(sizes)-1].W)
	d := duration(f)
	length := min(sel.Duration, d)
	from := max(0, min(sel.Start, d-length))
	clip := filepath.Join(dir, "section.mp4")
	start, err := snippet(ctx, e.c.Store, item, r, from, from+length, clip)
	if err != nil {
		return err
	}
	webp, mp4, err := renderPreview(ctx, clip, from-start, length, sizes, dir, e.c.Threads)
	if err != nil {
		return err
	}
	res := &media.HoverPreviewResult{Of: sel.Key(), Source: f.Source(), Recipe: PreviewRecipe,
		Version: media.HoverPreviewVersion(sel.Key(), f.Source(), PreviewRecipe)}
	for i, s := range sizes {
		for _, out := range []struct {
			path, ctype string
			mp4         bool
		}{{webp[i], "image/webp", false}, {mp4[i], "video/mp4", true}} {
			body, err := os.ReadFile(out.path)
			if err != nil {
				return err
			}
			if _, err := e.c.Store.Put(ctx, item.HoverPreviewOutput(s.W, out.mp4), bytes.NewReader(body), int64(len(body)),
				media.PutOptions{ContentType: out.ctype, CacheControl: "no-cache", Metadata: map[string]string{layout.VersionMeta: res.Version}}); err != nil {
				return err
			}
		}
		res.Outputs = append(res.Outputs, s)
	}
	for _, w := range media.HoverPreviewWidths {
		if slices.ContainsFunc(sizes, func(s media.Dims) bool { return s.W == w }) {
			continue
		}
		for _, isMP4 := range []bool{false, true} {
			if err := e.c.Store.Delete(ctx, item.HoverPreviewOutput(w, isMP4)); err != nil && !errors.Is(err, media.ErrNotFound) {
				return err
			}
		}
	}
	e.c.Logger.InfoContext(ctx, "media/video: hover preview", "ref", ref.String(), "file", f.Name, "start", from, "duration", length)
	if err := ms.UpdateHoverPreview(ctx, ref, func(cur *media.HoverPreviewRecord) (*media.HoverPreviewRecord, error) {
		if cur == nil || cur.Key() != sel.Key() {
			return nil, media.ErrSuperseded
		}
		cur.Result = res
		return cur, nil
	}); err != nil {
		return err
	}
	// The host's process job publishes it (media.Jobs.Publish) as the item's exposure allows.
	if e.c.Slots == nil {
		e.c.Logger.WarnContext(ctx, "media/video: no Config.Slots; hover preview not published", "ref", ref.String())
		return nil
	}
	return e.c.Slots.Enqueue(ctx, media.ProcessJob{Ref: ref.Content(), Slot: media.HoverPreview})
}
