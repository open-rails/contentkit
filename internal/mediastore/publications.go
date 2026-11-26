package mediastore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// cachedImage is one upload's public preset as the item cache stores it.
type cachedImage struct {
	Name       string            `json:"name"` // the upload's {name}
	Path       string            `json:"path"`
	Preset     string            `json:"preset"`
	Renditions []cachedRendition `json:"renditions"`
}

type cachedRendition struct {
	Hash string `json:"hash"` // its name in public/
	W    int    `json:"w"`
	H    int    `json:"h"`
}

// publicImages lists m's current public images in reading order: uploads in
// manifest order, each one's presets in declaration order. Only outputs
// already in public/ count.
func (k *RegisteredKind) publicImages(m *Manifest) []cachedImage {
	out := []cachedImage{}
	if m.Deleted != nil {
		return out
	}
	for _, f := range m.Files {
		if !f.IsUpload() {
			continue
		}
		for _, p := range k.PublicFor(f.Path) {
			img := cachedImage{Name: k.NameOf(f.Path), Path: f.Path, Preset: p.Name}
			for _, o := range k.PublicOutputs(m, f, p) {
				if area, hash, ok := layout.SplitBlobRef(o.Blob); ok && area == layout.AreaPublic {
					img.Renditions = append(img.Renditions, cachedRendition{Hash: hash, W: o.W, H: o.H})
				}
			}
			if len(img.Renditions) > 0 {
				out = append(out, img)
			}
		}
	}
	return out
}

// PublicImages are m's current public images with their URLs.
func (r *Registry) PublicImages(item Item, m *Manifest) []media.PublicImage {
	return r.images(item.Ref(), item.Kind().publicImages(m))
}

func (r *Registry) images(ref contentref.ContentRef, rows []cachedImage) []media.PublicImage {
	var out []media.PublicImage
	for _, row := range rows {
		image := media.PublicImage{From: row.Path, Preset: row.Preset}
		for _, rend := range row.Renditions {
			image.Renditions = append(image.Renditions, media.PublicRendition{URL: r.PublicURL(ref, rend.Hash), W: rend.W, H: rend.H})
		}
		out = append(out, image)
	}
	return out
}

// Images answers every query from the item cache in one read per schema:
// per query, in order, the matching images in the item's order. Hosts build
// card, avatar and inline image URLs from it.
func (c *PGCache) Images(ctx context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error) {
	out := make([][]media.PublicImage, len(qs))
	refs := make([]contentref.ContentRef, len(qs))
	for i, q := range qs {
		item, err := c.reg.Item(q.Ref)
		if err != nil {
			return nil, err
		}
		if q.Preset != "" && item.Kind().public(q.Preset) == nil {
			return nil, fmt.Errorf("media: kind %q has no public preset %q", item.Kind().Name, q.Preset)
		}
		refs[i] = q.Ref
	}
	rows, err := c.read(ctx, refs)
	if err != nil {
		return nil, err
	}
	for i, q := range qs {
		row, ok := rows[q.Ref.Key()]
		if !ok || row.Deleted {
			continue
		}
		var match []cachedImage
		for _, img := range row.Images {
			if (q.Name == "" || img.Name == q.Name) && (q.Preset == "" || img.Preset == q.Preset) {
				match = append(match, img)
			}
		}
		out[i] = c.reg.images(q.Ref, match)
	}
	return out, nil
}

// Avatars returns, per id in order, the person's current avatar
// (Config.Avatars), else the kind's default image; none without
// Config.Avatars or for an invalid id.
func (c *PGCache) Avatars(ctx context.Context, ids ...string) ([]media.PublicImage, error) {
	a := c.reg.cfg.Avatars
	if a == (media.Avatars{}) {
		return nil, nil
	}
	qs := make([]media.ImageQuery, 0, len(ids))
	at := make([]int, 0, len(ids))
	for i, id := range ids {
		if ref, err := c.reg.Ref(a.Kind, id); err == nil {
			qs = append(qs, media.ImageQuery{Ref: ref, Preset: a.Preset})
			at = append(at, i)
		}
	}
	found, err := c.Images(ctx, qs...)
	if err != nil {
		return nil, err
	}
	out := make([]media.PublicImage, len(ids))
	for i := range out {
		out[i] = media.PublicImage{Preset: a.Preset}
	}
	for n, i := range at {
		if len(found[n]) > 0 {
			out[i] = found[n][0]
		} else if d, ok := c.reg.DefaultImage(ctx, qs[n].Ref, a.Preset); ok {
			out[i] = d
		}
	}
	return out, nil
}

// DefaultImage is preset's default image for ref's kind, rendered into the
// kind's _default item (image.PublishDefaults). From is empty.
func (r *Registry) DefaultImage(ctx context.Context, ref contentref.ContentRef, preset string) (media.PublicImage, bool) {
	item, err := r.Item(ref)
	if err != nil {
		return media.PublicImage{}, false
	}
	p := item.Kind().public(preset)
	if p == nil {
		return media.PublicImage{}, false
	}
	return r.defaultImage(ctx, item.Kind(), p)
}

// hasDefault reports a default image for the preset: one declaring Default,
// not per upload ({name}).
func hasDefault(p *media.Public) bool {
	return p.Default != "" && !strings.Contains(p.To, "{name}")
}

// defaultImage reads the kind's defaults manifest: the preset's outputs of
// its Default path.
func (m *Manifests) defaultImage(ctx context.Context, k *RegisteredKind, p *media.Public) (media.PublicImage, bool) {
	item := k.DefaultsItem()
	cur, _, err := m.get(ctx, item.ManifestKey())
	if err != nil {
		return media.PublicImage{}, false
	}
	image := media.PublicImage{Preset: p.Name}
	for _, o := range cur.Outputs(p.Default, p.Name) {
		if area, hash, ok := layout.SplitBlobRef(o.Blob); ok && area == layout.AreaPublic {
			image.Renditions = append(image.Renditions, media.PublicRendition{URL: m.reg.publicURL(item, hash), W: o.W, H: o.H})
		}
	}
	return image, len(image.Renditions) > 0
}

func decodeImages(b []byte) ([]cachedImage, error) {
	var out []cachedImage
	if len(b) == 0 {
		return out, nil
	}
	err := json.Unmarshal(b, &out)
	return out, err
}
