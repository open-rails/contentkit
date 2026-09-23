package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// Public slot outputs record what they were derived from.
const (
	metaSource = "source" // ETag of the slot original
	metaSpec   = "spec"
)

// slot re-encodes a slot original into each output at public/{output}.webp,
// in place, served no-cache so the next view revalidates its ETag. An output
// already derived from this original and spec is left alone. Writes are
// conditional on the output's previous ETag, so a job holding an older
// original never overwrites a newer one's output.
func (p *Processor) slot(ctx context.Context, item media.Item, slot string) error {
	key, err := item.SlotOriginal(slot)
	if err != nil {
		return err
	}
	outputs := item.Kind().Slots[slot].Outputs
	conditional := p.c.Store.Capabilities().ConditionalPut
	for range 8 {
		orig, err := p.c.Store.Head(ctx, key)
		if errors.Is(err, media.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		source := strings.Trim(orig.ETag, `"`)
		stale := map[string]string{} // output key → its current ETag ("" when absent)
		specs := map[string]media.Spec{}
		for out, s := range outputs {
			outKey, err := item.Public(out)
			if err != nil {
				return err
			}
			obj, err := p.c.Store.Head(ctx, outKey)
			if err != nil && !errors.Is(err, media.ErrNotFound) {
				return err
			}
			if err == nil && obj.Metadata[metaSource] == source && obj.Metadata[metaSpec] == s.Hash() {
				continue
			}
			stale[outKey], specs[outKey] = obj.ETag, s
		}
		if len(stale) == 0 {
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
		conflict := false
		for outKey, prev := range stale {
			s := specs[outKey]
			out, err := encode(src, s)
			if err != nil {
				p.failed(ctx, item.Ref(), slot, err)
				return nil
			}
			opts := media.PutOptions{ContentType: "image/webp", CacheControl: "no-cache",
				Metadata: map[string]string{metaSource: source, metaSpec: s.Hash()}}
			if conditional {
				if prev == "" {
					opts.IfNoneMatch = "*"
				} else {
					opts.IfMatch = prev
				}
			}
			_, err = p.c.Store.Put(ctx, outKey, bytes.NewReader(out), int64(len(out)), opts)
			if errors.Is(err, media.ErrPreconditionFailed) {
				conflict = true
				continue
			} else if err != nil {
				return err
			}
		}
		_ = conflict // re-check: a newer original may have landed meanwhile
	}
	return fmt.Errorf("media/image: slot %s of %s kept changing", slot, item.Ref())
}
