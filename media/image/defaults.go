package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"path"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// DefaultPublication is the complete fallback selection for a deployment.
// Written contains new immutable objects; Defaults includes existing ones too.
type DefaultPublication struct {
	Written  []string
	Defaults []layout.Default
}

// PublishDefaults renders to content-addressed, create-only default objects.
// Deploy its mappings only after success; overlapping deploys never overwrite
// each other's defaults. The gateway needs no mutable pointer or manifest read.
func PublishDefaults(ctx context.Context, store media.Store, reg *media.Registry) (DefaultPublication, error) {
	var result DefaultPublication
	if !store.Capabilities().ConditionalPut {
		return result, media.ErrConditionalPutRequired
	}
	if err := start(); err != nil {
		return result, fmt.Errorf("media/image: libvips: %w", err)
	}
	cfg := reg.Config()
	for _, k := range cfg.Kinds {
		d := layout.Default{Namespace: k.NS(), Kind: k.Name, Files: map[string]string{}}
		for i := range k.Public {
			p := &k.Public[i]
			if p.Default == "" {
				continue
			}
			if k.Defaults == nil {
				return result, fmt.Errorf("media/image: kind %s names default %s but has no Defaults", k.Name, p.Default)
			}
			src, err := fs.ReadFile(k.Defaults, p.Default)
			if err != nil {
				return result, fmt.Errorf("media/image: default %s: %w", p.Default, err)
			}
			typ := mime.TypeByExtension(path.Ext(p.Default))
			names := k.PublicNames(nil, p, "")
			outs, _, err := encodePublic(src, typ, p, names, nil, rules{maxPixels: 100_000_000, maxFrames: 1000, maxSeconds: 60})
			if err != nil {
				return result, fmt.Errorf("media/image: default %s: %w", p.Default, err)
			}
			for _, n := range names {
				out := outs[n]
				sum := sha(out.webp)
				name := layout.SHA256Name(sum) + ".webp"
				key := k.DefaultKey(name)
				if _, err := store.Put(ctx, key, bytes.NewReader(out.webp), int64(len(out.webp)), media.PutOptions{
					IfNoneMatch: "*", ContentType: "image/webp", ChecksumSHA256: sum}); errors.Is(err, media.ErrPreconditionFailed) {
					body, obj, err := store.Get(ctx, key, media.GetOptions{})
					if err != nil {
						return result, err
					}
					got, readErr := io.ReadAll(io.LimitReader(body, int64(len(out.webp))+1))
					closeErr := body.Close()
					if err := errors.Join(readErr, closeErr); err != nil {
						return result, err
					}
					if obj.ContentType != "image/webp" || !bytes.Equal(got, out.webp) {
						return result, fmt.Errorf("media/image: immutable default %s has different bytes or type", key)
					}
				} else if err != nil {
					return result, err
				} else {
					result.Written = append(result.Written, key)
				}
				if previous := d.Files[n]; previous != "" && previous != name {
					return result, fmt.Errorf("media/image: conflicting defaults for %s/%s/%s", d.Namespace, d.Kind, n)
				}
				d.Files[n] = name
			}
		}
		if len(d.Files) != 0 {
			result.Defaults = append(result.Defaults, d)
		}
	}
	return result, nil
}
