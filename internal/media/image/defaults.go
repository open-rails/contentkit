package image

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"mime"
	"path"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// PublishDefaults renders every kind's default public images into its
// _default item: each output under its hash in public/, then the item's
// manifest naming them (Registry.DefaultImage reads it). It returns the kinds
// it wrote. Rendering again writes the same names and changes nothing.
func PublishDefaults(ctx context.Context, manifests *mediastore.Manifests) ([]string, error) {
	if err := start(); err != nil {
		return nil, fmt.Errorf("media/image: libvips: %w", err)
	}
	store := manifests.Store()
	var written []string
	for _, k := range manifests.Registry().Kinds() {
		item := k.DefaultsItem()
		outputs := map[string][]media.File{} // by preset
		for i := range k.Public {
			p := &k.Public[i]
			if p.Default == "" {
				continue
			}
			if k.Defaults == nil {
				return written, fmt.Errorf("media/image: kind %s names default %s but has no Defaults", k.Name, p.Default)
			}
			src, err := fs.ReadFile(k.Defaults, p.Default)
			if err != nil {
				return written, fmt.Errorf("media/image: default %s: %w", p.Default, err)
			}
			typ := mime.TypeByExtension(path.Ext(p.Default))
			names := k.PublicNames(p, "")
			outs, _, err := encodePublic(src, typ, p, names, nil, rules{maxPixels: 100_000_000, maxFrames: 1000, maxSeconds: 60})
			if err != nil {
				return written, fmt.Errorf("media/image: default %s: %w", p.Default, err)
			}
			for _, n := range names {
				out := outs[n]
				sum := sha(out.webp)
				blob := layout.BlobRef(layout.AreaPublic, sum)
				key, _ := item.Blob(blob)
				if _, err := store.Put(ctx, key, bytes.NewReader(out.webp), int64(len(out.webp)), media.PutOptions{ContentType: "image/webp", ChecksumSHA256: sum}); err != nil {
					return written, err
				}
				outputs[p.Name] = append(outputs[p.Name], media.File{Path: n, Blob: blob, Type: "image/webp", Size: int64(len(out.webp)),
					W: out.dims.W, H: out.dims.H, From: p.Default, Preset: p.Name, FP: Recipe})
			}
		}
		if len(outputs) == 0 {
			continue
		}
		_, err := manifests.EditItem(ctx, item, mediastore.EditOptions{}, func(m *mediastore.Manifest) error {
			for i := range k.Public {
				p := &k.Public[i]
				if outs, ok := outputs[p.Name]; ok {
					if err := m.SetOutputs(p.Default, p.Name, outs); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return written, err
		}
		written = append(written, k.Name)
	}
	return written, nil
}
