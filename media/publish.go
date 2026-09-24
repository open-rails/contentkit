package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Exposure is what a video item publishes to public/, where anyone can fetch
// it without a token. Its poster and hover preview are rendered to editor/
// and copied to public/ only as its Exposure allows.
type Exposure struct {
	Poster       bool `json:"poster"`
	HoverPreview bool `json:"hover_preview"`
}

// ExposurePolicy decides an item's Exposure from its anonymous Resolution;
// hosts may vary it per item (a paid post's teaser, say).
type ExposurePolicy func(ctx context.Context, ref contentref.ContentRef, anonymous access.Resolution) Exposure

// DefaultExposure publishes nothing for an item anonymous viewers cannot see
// (a draft, a deleted item), everything for one they fully can, and the
// poster alone as the teaser of any other visible item (paid, preview-cut).
func DefaultExposure(_ context.Context, _ contentref.ContentRef, res access.Resolution) Exposure {
	switch {
	case !res.Visible:
		return Exposure{}
	case res.Full():
		return Exposure{Poster: true, HoverPreview: true}
	}
	return Exposure{Poster: true}
}

const exposureRecord = "exposure"

// ExposureRecord is originals/exposure.json, the Exposure last published.
func (i Item) ExposureRecord() string { return i.OriginalsPrefix() + exposureRecord + slotRecordExt }

// Exposure is the item's published Exposure (zero before the first publish).
func (m *Manifests) Exposure(ctx context.Context, ref contentref.ContentRef) (Exposure, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return Exposure{}, err
	}
	body, _, err := m.raw(ctx, item.ExposureRecord())
	if errors.Is(err, ErrNotFound) {
		return Exposure{}, nil
	} else if err != nil {
		return Exposure{}, err
	}
	var e Exposure
	if err := json.Unmarshal(body, &e); err != nil {
		return Exposure{}, fmt.Errorf("media: decode exposure record: %w", err)
	}
	return e, nil
}

// gatedOutput reports a poster or hover-preview output name in public/ or
// editor/ ("poster_480.webp", "hover_preview_320.mp4") and which it is.
func gatedOutput(name string) (poster, preview bool) {
	base, ok := strings.CutSuffix(name, layout.PublicExt)
	if !ok {
		base, ok = strings.CutSuffix(name, layout.PublicMP4Ext)
		if !ok {
			return false, false
		}
		return false, strings.HasPrefix(base, HoverPreview+"_")
	}
	return strings.HasPrefix(base, PosterSlot+"_"), strings.HasPrefix(base, HoverPreview+"_")
}

// Publish brings a video item's public/ poster and hover preview to its
// Exposure: it resolves the item for an anonymous actor, applies
// JobsConfig.Exposure, copies the allowed outputs from editor/ and deletes
// the rest. It re-resolves after writing and repeats until the Exposure
// holds, so a publish racing a visibility change ends at the newer one.
// Other kinds are left alone. Without JobsConfig.Resolver nothing is public.
func (j *Jobs) Publish(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.cfg.Kinds.Item(ref.Content())
	if err != nil || item.Kind().Video == nil {
		return err
	}
	exp, err := j.exposure(ctx, item.Ref())
	if err != nil {
		return err
	}
	for range 4 {
		if err := j.mirror(ctx, item, exp); err != nil {
			return err
		}
		now, err := j.exposure(ctx, item.Ref())
		if err != nil {
			return err
		}
		if now == exp {
			return nil
		}
		exp = now
	}
	return fmt.Errorf("media: exposure of %s kept changing", ref)
}

func (j *Jobs) exposure(ctx context.Context, ref contentref.ContentRef) (Exposure, error) {
	if j.cfg.Resolver == nil {
		return Exposure{}, nil
	}
	res, err := access.ResolveOne(ctx, j.cfg.Resolver, ref, access.Actor{Anonymous: true})
	if err != nil {
		return Exposure{}, fmt.Errorf("media: resolve %s for publishing: %w", ref, err)
	}
	return j.cfg.Exposure(ctx, ref, res), nil
}

// mirror makes public/'s poster and hover-preview outputs the exposed subset
// of editor/'s, then records exp.
func (j *Jobs) mirror(ctx context.Context, item Item, exp Exposure) error {
	want := map[string]Object{} // name → staged output
	for o, err := range j.cfg.Store.List(ctx, item.EditorPrefix()) {
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(o.Key, item.EditorPrefix())
		if poster, preview := gatedOutput(name); poster && exp.Poster || preview && exp.HoverPreview {
			want[name] = o
		}
	}
	for o, err := range j.cfg.Store.List(ctx, item.PublicPrefix()) {
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(o.Key, item.PublicPrefix())
		if poster, preview := gatedOutput(name); !poster && !preview {
			continue
		}
		if _, ok := want[name]; !ok {
			if err := j.cfg.Store.Delete(ctx, o.Key); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	for name, staged := range want {
		if err := j.copyOutput(ctx, staged.Key, item.PublicPrefix()+name); err != nil {
			return err
		}
	}
	return j.recordExposure(ctx, item, exp)
}

// copyOutput copies src to dst unless dst already holds the same bytes (ETag).
func (j *Jobs) copyOutput(ctx context.Context, src, dst string) error {
	rc, obj, err := j.cfg.Store.Get(ctx, src, GetOptions{})
	if errors.Is(err, ErrNotFound) {
		return nil // replaced meanwhile; its job publishes again
	} else if err != nil {
		return err
	}
	defer rc.Close()
	if cur, err := j.cfg.Store.Head(ctx, dst); err == nil && obj.ETag != "" && cur.ETag == obj.ETag && cur.Size == obj.Size {
		return nil
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	_, err = j.cfg.Store.Put(ctx, dst, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: obj.ContentType, CacheControl: "no-cache"})
	return err
}

// recordExposure stores exp; nothing published is no record, so publishing
// a deleted item never writes into its erased folder.
func (j *Jobs) recordExposure(ctx context.Context, item Item, exp Exposure) error {
	key := item.ExposureRecord()
	if exp == (Exposure{}) {
		if err := j.cfg.Store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	}
	body, _ := json.Marshal(exp)
	if rc, _, err := j.cfg.Store.Get(ctx, key, GetOptions{}); err == nil {
		cur, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr == nil && bytes.Equal(cur, body) {
			return nil
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := j.cfg.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "application/json"})
	return err
}

// PublishTx enqueues Publish for each item in the host's transaction. Call it
// whenever what anonymous viewers may see of an item changes: publish,
// unpublish, soft delete, restore, a price or access change.
func (j *Jobs) PublishTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error {
	c, err := j.bound()
	if err != nil {
		return err
	}
	params := make([]river.InsertManyParams, 0, len(refs))
	for _, ref := range refs {
		if _, err := j.cfg.Kinds.Item(ref.Content()); err != nil {
			return err
		}
		// Not unique: River's uniqueness always covers running jobs, and a
		// running publish may have resolved before this change. Publish is
		// idempotent, so a duplicate costs one listing.
		params = append(params, river.InsertManyParams{Args: publishArgs{Ref: ref.Content()}, InsertOpts: j.opts(nil)})
	}
	if len(params) == 0 {
		return nil
	}
	_, err = c.InsertManyTx(ctx, tx, params)
	return err
}

type publishArgs struct {
	Ref contentref.ContentRef `json:"ref"`
}

func (publishArgs) Kind() string { return "contentkit_media_publish" }

type publishWorker struct {
	river.WorkerDefaults[publishArgs]
	j *Jobs
}

func (w *publishWorker) Timeout(*river.Job[publishArgs]) time.Duration { return 5 * time.Minute }

func (w *publishWorker) Work(ctx context.Context, job *river.Job[publishArgs]) error {
	if _, err := w.j.cfg.Kinds.Item(job.Args.Ref); err != nil {
		return river.JobCancel(err)
	}
	return w.j.Publish(ctx, job.Args.Ref)
}
