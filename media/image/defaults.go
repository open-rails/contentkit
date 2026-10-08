package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"path"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// PublishDefaults renders every public preset's Default (a file in its
// kind's Defaults) to the kind's _default item at every width,
// where the media gateway serves a missing public name from. An unchanged
// default is not written again. It returns the keys written, for a CDN
// purge. Apps run it from their deploy step.
func PublishDefaults(ctx context.Context, store media.Store, reg *media.Registry) ([]string, error) {
	if err := start(); err != nil {
		return nil, fmt.Errorf("media/image: libvips: %w", err)
	}
	cfg := reg.Config()
	var written []string
	for _, k := range cfg.Kinds {
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
			// The default's fingerprint: its bytes as the blob, the preset's spec.
			fp := publicFP(media.File{Blob: layout.SHA256Name(sha(src))}, p)
			names := k.PublicNames(nil, p, "")
			outs, _, err := encodePublic(src, typ, p, names, nil, rules{maxPixels: 100_000_000, maxFrames: 1000, maxSeconds: 60})
			if err != nil {
				return written, fmt.Errorf("media/image: default %s: %w", p.Default, err)
			}
			for _, n := range names {
				key := k.DefaultKey(n)
				if obj, err := store.Head(ctx, key); err == nil && obj.Metadata["fp"] == fp {
					continue
				} else if err != nil && !errors.Is(err, media.ErrNotFound) {
					return written, err
				}
				out := outs[n]
				if _, err := store.Put(ctx, key, bytes.NewReader(out.webp), int64(len(out.webp)), media.PutOptions{
					ContentType: "image/webp", ChecksumSHA256: sha(out.webp), Metadata: map[string]string{"fp": fp}}); err != nil {
					return written, err
				}
				written = append(written, key)
			}
		}
	}
	return written, nil
}
