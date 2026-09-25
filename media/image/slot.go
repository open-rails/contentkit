package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// errSuperseded aborts recording a result for a slot record that changed meanwhile.
var errSuperseded = errors.New("media/image: slot record changed")

// slot renders a registered slot or an inline image.
func (p *Processor) slot(ctx context.Context, item media.Item, slot string) error {
	if spec, ok := item.Kind().Slots[slot]; ok {
		return p.render(ctx, item, slot, spec, func(src []byte, typ string, rec *media.SlotRecord) (map[int]slotOutput, media.Dims, error) {
			return encodeSlot(src, typ, spec, rec.Edit, p.rules(spec.Animation))
		})
	}
	if item.Inline(slot) {
		s := *item.Kind().Inline
		return p.render(ctx, item, slot, media.InlineSlot(s), func(src []byte, typ string, _ *media.SlotRecord) (map[int]slotOutput, media.Dims, error) {
			info, err := probe(src, typ, p.rules(item.Kind().Animation))
			if err != nil {
				return nil, media.Dims{}, err
			}
			out, d, err := encode(src, typ, s, nil)
			return map[int]slotOutput{s.Width: {webp: out, dims: d}}, info.dims(), err
		})
	}
	return fmt.Errorf("media/image: kind %q has no slot %q", item.Kind().Name, slot)
}

type slotEncoder func(src []byte, typ string, rec *media.SlotRecord) (map[int]slotOutput, media.Dims, error)

// render brings a committed slot's renditions to its record: the original,
// EXIF-oriented and edited, encoded at every rung (capped at the edited
// width) to private/{sha256}, copied to public/ unless the item is hidden,
// then recorded. Every pass re-checks the record, so jobs holding an older
// commit or edit converge on the newest; the replaced renditions are left to
// the sweep. Hooks.SlotEncoded reports the new outputs.
func (p *Processor) render(ctx context.Context, item media.Item, slot string, spec media.Slot, enc slotEncoder) error {
	ref := item.Ref().Content()
	for range 8 {
		rec, err := p.c.Manifests.Slot(ctx, ref, slot)
		if errors.Is(err, media.ErrNotFound) {
			return nil // never committed
		} else if err != nil {
			return err
		}
		if rec.Original == "" {
			return nil // a poster frame not grabbed yet
		}
		fp := rec.Fingerprint(spec)
		if rec.Result != nil && rec.Result.Of == fp {
			return nil // current, or failed for this commit and edit
		}
		key, err := item.Original(rec.Original)
		if err != nil {
			return err
		}
		src, got, err := p.read(ctx, key)
		if err != nil {
			return err
		}
		res := &media.SlotResult{Of: fp, Source: rec.Original}
		outs, dims, err := enc(src, got.ContentType, rec)
		res.Dims = dims
		if err != nil {
			if !isPermanent(err) {
				return err
			}
			if prev := rec.Result; prev != nil {
				res.Outputs = prev.Outputs
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
		var blobs []string
		for _, w := range spec.Widths {
			out, fits := outs[w]
			if !fits {
				continue
			}
			blob, err := p.putBlob(ctx, item, bytes.NewReader(out.webp), int64(len(out.webp)), sha(out.webp), "image/webp")
			if err != nil {
				return err
			}
			blobs = append(blobs, blob)
			res.Outputs = append(res.Outputs, media.SlotRendition{Rung: w, W: out.dims.W, H: out.dims.H, Blob: blob, Size: int64(len(out.webp))})
		}
		if err := p.prepublish(ctx, item, blobs); err != nil {
			return err
		}
		if err := p.record(ctx, ref, slot, spec, fp, res); errors.Is(err, errSuperseded) {
			continue
		} else if err != nil {
			return err
		}
		if err := p.syncPublic(ctx, ref); err != nil {
			return err
		}
		if p.c.Hooks.SlotEncoded != nil && len(res.Outputs) > 0 {
			p.c.Hooks.SlotEncoded(ctx, ref, slot, res.Listing(spec))
		}
		return nil
	}
	return fmt.Errorf("media/image: slot %s of %s kept changing", slot, item.Ref())
}

// prepublish copies new renditions to public/ before they are recorded,
// unless the item is hidden, so a listed public URL never misses.
func (p *Processor) prepublish(ctx context.Context, item media.Item, blobs []string) error {
	root, _, err := p.c.Manifests.Root(ctx, item.Ref())
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return err
	}
	if root == nil || root.Hidden {
		return nil
	}
	for _, b := range blobs {
		src, _ := item.Private(b)
		dst, _ := item.Public(b)
		if _, err := p.c.Store.Head(ctx, dst); err == nil {
			continue
		} else if !errors.Is(err, media.ErrNotFound) {
			return err
		}
		if _, err := p.c.Store.Copy(ctx, src, dst, media.CopyOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// syncPublic brings public/ to the recorded state (media.Manifests.SyncPublic).
func (p *Processor) syncPublic(ctx context.Context, ref contentref.ContentRef) error {
	removed, err := p.c.Manifests.SyncPublic(ctx, ref)
	if len(removed) > 0 && p.c.Hooks.PublicRemoved != nil {
		p.c.Hooks.PublicRemoved(ctx, ref, removed)
	}
	return err
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
