package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Inline image outputs record what they were derived from.
const (
	metaSource = "source" // ETag of the inline original
	metaSpec   = "spec"
)

// errSuperseded aborts recording a result for a slot record that changed meanwhile.
var errSuperseded = errors.New("media/image: slot record changed")

// slot re-encodes a registered slot or an inline image.
func (p *Processor) slot(ctx context.Context, item media.Item, slot string) error {
	if spec, ok := item.Kind().Slots[slot]; ok {
		return p.registered(ctx, item, slot, spec)
	}
	if item.Inline(slot) {
		return p.inline(ctx, item, slot)
	}
	return fmt.Errorf("media/image: kind %q has no slot %q", item.Kind().Name, slot)
}

// registered brings a committed slot's outputs to its record: the committed
// original, EXIF-oriented and edited, encoded at each width that fits and
// written in place at Item.SlotOutput (no-cache, tagged with the
// record's fingerprint). Widths that do not fit, or that the slot no longer
// declares, are deleted. Output writes are conditional on the ETag seen, and
// every pass re-checks the current record, so jobs holding an older commit
// or edit converge on the newest. Hooks.SlotEncoded runs before the record
// that makes the encode visible, so a host refetching on it gets the new stamp;
// a pass whose stamp lost to a newer record re-reports the newest.
func (p *Processor) registered(ctx context.Context, item media.Item, slot string, spec media.Slot) error {
	key, _ := item.SlotOriginal(slot)
	ref := item.Ref().Content()
	conditional := p.c.Store.Capabilities().ConditionalPut
	stale := false
	for range 8 {
		rec, err := p.c.Manifests.Slot(ctx, ref, slot)
		if errors.Is(err, media.ErrNotFound) {
			return nil // never committed
		} else if err != nil {
			return err
		}
		fp := rec.Fingerprint(spec)
		etags, current, err := p.outputs(ctx, item, slot, spec, fp, rec.Result)
		if err != nil {
			return err
		}
		if current {
			if stale && rec.Result.Version != "" {
				p.slotEncoded(ctx, ref, slot, rec.Result)
			}
			return nil
		}
		src, got, err := p.read(ctx, key)
		if errors.Is(err, media.ErrNotFound) || (err == nil && got.ETag != rec.Original) {
			return nil // replaced by an upload not committed yet; its commit re-enqueues
		} else if err != nil {
			return err
		}
		res := &media.SlotResult{Of: fp, Source: rec.Original}
		outs, dims, err := encodeSlot(src, got.ContentType, spec, rec.Edit, p.c.MaxPixels)
		res.Dims = dims
		if err != nil {
			if !isPermanent(err) {
				return err
			}
			if prev := rec.Result; prev != nil {
				res.Version, res.Outputs = prev.Version, prev.Outputs
			}
			res.Error = err.Error()
			if ie := media.AsImageError(err); ie != nil {
				res.Error, res.Code, res.Details = ie.Message, ie.Code, &ie.Details
			}
			if err := p.record(ctx, ref, slot, spec, fp, res); errors.Is(err, errSuperseded) {
				continue
			} else if err != nil {
				return err
			}
			p.failed(ctx, item.Ref(), slot, err)
			return nil
		}
		res.Version = fp
		for _, w := range spec.Widths {
			outKey, _ := item.SlotOutput(slot, w)
			out, fits := outs[w]
			if !fits {
				if etags[w] != "" {
					if err := p.c.Store.Delete(ctx, outKey); err != nil {
						return err
					}
				}
				continue
			}
			opts := media.PutOptions{ContentType: "image/webp", CacheControl: "no-cache", Metadata: map[string]string{layout.VersionMeta: fp}}
			if conditional {
				if etags[w] == "" {
					opts.IfNoneMatch = "*"
				} else {
					opts.IfMatch = etags[w]
				}
			}
			if _, err := p.c.Store.Put(ctx, outKey, bytes.NewReader(out.webp), int64(len(out.webp)), opts); err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
				return err
			}
			res.Outputs = append(res.Outputs, out.dims)
		}
		if err := p.dropRetired(ctx, item, slot, spec); err != nil {
			return err
		}
		// A conflicting write or a newer record is settled by the next pass.
		p.slotEncoded(ctx, ref, slot, res)
		if err := p.record(ctx, ref, slot, spec, fp, res); errors.Is(err, errSuperseded) {
			stale = true
		} else if err != nil {
			return err
		}
	}
	return fmt.Errorf("media/image: slot %s of %s kept changing", slot, item.Ref())
}

func (p *Processor) slotEncoded(ctx context.Context, ref contentref.ContentRef, slot string, res *media.SlotResult) {
	if p.c.Hooks.SlotEncoded == nil {
		return
	}
	p.c.Hooks.SlotEncoded(ctx, ref, slot, media.NewSlotStamp(res.Version, res.Outputs))
}

// outputs heads the slot's outputs: their ETags ("" when absent) and whether
// they and the recorded result already match fp.
func (p *Processor) outputs(ctx context.Context, item media.Item, slot string, spec media.Slot, fp string, res *media.SlotResult) (map[int]string, bool, error) {
	etags := map[int]string{}
	if res != nil && res.Of == fp && res.Error != "" {
		return etags, true, nil // failed for this commit, edit and spec; a new one retries
	}
	current := res != nil && res.Of == fp
	var want []int
	if current {
		for _, o := range res.Outputs {
			want = append(want, o.W)
		}
	}
	for _, w := range spec.Widths {
		key, _ := item.SlotOutput(slot, w)
		obj, err := p.c.Store.Head(ctx, key)
		if errors.Is(err, media.ErrNotFound) {
			current = current && !slices.Contains(want, w)
			continue
		} else if err != nil {
			return nil, false, err
		}
		etags[w] = obj.ETag
		current = current && slices.Contains(want, w) && obj.Metadata[layout.VersionMeta] == fp
	}
	return etags, current, nil
}

// record stores res unless the record moved past fp.
func (p *Processor) record(ctx context.Context, ref contentref.ContentRef, slot string, spec media.Slot, fp string, res *media.SlotResult) error {
	return p.c.Manifests.UpdateSlot(ctx, ref, slot, func(rec *media.SlotRecord) error {
		if rec.Fingerprint(spec) != fp {
			return errSuperseded
		}
		rec.Result = res
		return nil
	})
}

// dropRetired deletes outputs of widths the slot no longer declares.
func (p *Processor) dropRetired(ctx context.Context, item media.Item, slot string, spec media.Slot) error {
	first, err := item.SlotOutput(slot, spec.Widths[0])
	if err != nil {
		return err
	}
	prefix := first[:strings.LastIndexByte(first, '/')+1] + slot + "_"
	for obj, err := range p.c.Store.List(ctx, prefix) {
		if err != nil {
			return err
		}
		n, ok := strings.CutSuffix(strings.TrimPrefix(obj.Key, prefix), layout.PublicExt)
		w, err := strconv.Atoi(n)
		if !ok || err != nil || strconv.Itoa(w) != n || slices.Contains(spec.Widths, w) {
			continue
		}
		if err := p.c.Store.Delete(ctx, obj.Key); err != nil {
			return err
		}
	}
	return nil
}

// inline re-encodes an inline original into public/{id}.webp with the kind's
// Inline spec, in place (no-cache). An output already derived from this
// original and spec is left alone; writes are conditional on the output's
// previous ETag.
func (p *Processor) inline(ctx context.Context, item media.Item, id string) error {
	key, _ := item.SlotOriginal(id)
	outKey, err := item.Public(id)
	if err != nil {
		return err
	}
	s := *item.Kind().Inline
	conditional := p.c.Store.Capabilities().ConditionalPut
	for range 8 {
		orig, err := p.c.Store.Head(ctx, key)
		if errors.Is(err, media.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		source := strings.Trim(orig.ETag, `"`)
		obj, err := p.c.Store.Head(ctx, outKey)
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
		if err == nil && obj.Metadata[metaSource] == source && obj.Metadata[metaSpec] == s.Hash() {
			return nil
		}
		src, got, err := p.read(ctx, key)
		if errors.Is(err, media.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if got.ETag != orig.ETag {
			continue // replaced while we looked
		}
		if _, _, err := probe(src, got.ContentType, p.c.MaxPixels); err != nil {
			p.failed(ctx, item.Ref(), id, err)
			return nil
		}
		out, err := encode(src, s, nil)
		if err != nil {
			p.failed(ctx, item.Ref(), id, err)
			return nil
		}
		opts := media.PutOptions{ContentType: "image/webp", CacheControl: "no-cache",
			Metadata: map[string]string{metaSource: source, metaSpec: s.Hash()}}
		if conditional {
			if obj.ETag == "" {
				opts.IfNoneMatch = "*"
			} else {
				opts.IfMatch = obj.ETag
			}
		}
		if _, err := p.c.Store.Put(ctx, outKey, bytes.NewReader(out), int64(len(out)), opts); err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
			return err
		}
	}
	return fmt.Errorf("media/image: inline image %s of %s kept changing", id, item.Ref())
}
