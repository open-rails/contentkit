package media

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// Every item's current publications are projected into Postgres
// (content_media_publications) in the journal transaction that settles the
// manifest write making them current, so a page of cards resolves its images
// with one indexed query instead of a manifest read per item. Cleanup asserts
// the projection before it retires a file, so a projected file is never one
// already selected for deletion.

// ImageQuery selects an item's current public images: all of them, or only
// those of a Preset, of an upload Name (its path's {name}, e.g. an inline
// image's "i-{uuid}"), or both.
type ImageQuery struct {
	Ref    contentref.ContentRef
	Preset string
	Name   string
}

// Images answers every query with one indexed Postgres read: per query, in
// order, the matching images in the item's order. Hosts build card, avatar
// and inline image URLs from it, never from a preset's template.
func (m *Manifests) Images(ctx context.Context, qs ...ImageQuery) ([][]PublicImage, error) {
	out := make([][]PublicImage, len(qs))
	if len(qs) == 0 {
		return out, nil
	}
	cols := make([][]string, 5)
	for _, q := range qs {
		item, err := m.reg.Item(q.Ref)
		if err != nil {
			return nil, err
		}
		if q.Preset != "" && item.Kind().public(q.Preset) == nil {
			return nil, fmt.Errorf("media: kind %q has no public preset %q", item.Kind().Name, q.Preset)
		}
		for i, v := range []string{q.Ref.TenantID, q.Ref.ContentKind, q.Ref.ContentID, q.Name, q.Preset} {
			cols[i] = append(cols[i], v)
		}
	}
	// Two arms keep a named lookup on the primary key's full prefix.
	const pick = `SELECT q.n, p.ordinal, p.upload_path, p.preset, p.renditions FROM q JOIN %[1]s p
ON p.tenant_id = q.tenant_id AND p.content_kind = q.content_kind AND p.content_id = q.content_id`
	t := m.journal.publications
	rows, err := m.journal.pool.Query(ctx, `WITH q AS (SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[])
WITH ORDINALITY AS q(tenant_id, content_kind, content_id, upload_name, preset, n))
`+fmt.Sprintf(pick, t)+` AND p.upload_name = q.upload_name WHERE q.upload_name <> '' AND (q.preset = '' OR p.preset = q.preset)
UNION ALL
`+fmt.Sprintf(pick, t)+` WHERE q.upload_name = '' AND (q.preset = '' OR p.preset = q.preset)
ORDER BY 1, 2, 3, 4`, cols[0], cols[1], cols[2], cols[3], cols[4])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			n, ordinal  int
			path, pset  string
			renditions  []byte
			projections []projectedRendition
		)
		if err := rows.Scan(&n, &ordinal, &path, &pset, &renditions); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(renditions, &projections); err != nil {
			return nil, fmt.Errorf("media: decode projected renditions: %w", err)
		}
		ref := qs[n-1].Ref
		image := PublicImage{From: path, Preset: pset}
		for _, r := range projections {
			image.Renditions = append(image.Renditions, PublicRendition{URL: m.reg.PublicURL(ref, r.Name), W: r.W, H: r.H})
		}
		out[n-1] = append(out[n-1], image)
	}
	return out, rows.Err()
}

// PublicImages returns ref's current public images (Images for one item). A
// missing item has none.
func (m *Manifests) PublicImages(ctx context.Context, ref contentref.ContentRef) ([]PublicImage, error) {
	images, err := m.Images(ctx, ImageQuery{Ref: ref})
	if err != nil {
		return nil, err
	}
	return images[0], nil
}

// PresetImages returns, per ref in order, its first current image of preset,
// else the preset's default (DefaultImage), else one without renditions. One
// query for the whole list.
func (m *Manifests) PresetImages(ctx context.Context, preset string, refs ...contentref.ContentRef) ([]PublicImage, error) {
	qs := make([]ImageQuery, len(refs))
	for i, ref := range refs {
		qs[i] = ImageQuery{Ref: ref, Preset: preset}
	}
	found, err := m.Images(ctx, qs...)
	if err != nil {
		return nil, err
	}
	out := make([]PublicImage, len(refs))
	for i, ref := range refs {
		switch def, ok := m.reg.DefaultImage(ref, preset); {
		case len(found[i]) > 0:
			out[i] = found[i][0]
		case ok:
			out[i] = def
		default:
			out[i] = PublicImage{Preset: preset}
		}
	}
	return out, nil
}

// DefaultImage is preset's default image for ref, which the media gateway
// serves while the item has no publication: only a preset declaring Default,
// without {n} or {name}. From is empty and heights follow the preset's
// aspect (0 without one).
func (r *Registry) DefaultImage(ref contentref.ContentRef, preset string) (PublicImage, bool) {
	item, err := r.Item(ref)
	if err != nil {
		return PublicImage{}, false
	}
	p := item.Kind().public(preset)
	if p == nil || p.Default == "" || p.First > 0 || strings.Contains(p.To, "{name}") {
		return PublicImage{}, false
	}
	image := PublicImage{Preset: preset}
	names := item.Kind().PublicNames(nil, p, "")
	for i, name := range names {
		w := p.Image.Width
		if len(p.Widths) > 0 {
			w = p.Widths[i]
		}
		h := 0
		if a := p.Image.Aspect; !a.Native() && w > 0 {
			h = a.Height(w)
		}
		image.Renditions = append(image.Renditions, PublicRendition{URL: r.PublicURL(ref, name), W: w, H: h})
	}
	return image, true
}

// publication is one current publication as the projection stores it.
type publication struct {
	Name       string               `json:"name"` // the upload's {name}
	Path       string               `json:"path"`
	Preset     string               `json:"preset"`
	Ordinal    int                  `json:"ordinal"`
	Generation *string              `json:"generation"` // null: legacy files at logical names
	Renditions []projectedRendition `json:"renditions"`
}

type projectedRendition struct {
	Name string `json:"name"` // the physical name in public/
	W    int    `json:"w"`
	H    int    `json:"h"`
}

// publications lists m's current publications in reading order: uploads in
// manifest order, each one's presets in declaration order.
func (k *Kind) publications(m *Manifest) []publication {
	var out []publication
	for _, f := range m.Files {
		if !f.IsUpload() {
			continue
		}
		for _, p := range k.PublicFor(f.Path) {
			pub, ok := k.Current(m, f, p)
			if !ok {
				continue
			}
			row := publication{Name: k.NameOf(f.Path), Path: f.Path, Preset: p.Name, Ordinal: len(out)}
			if pub.Generation != "" {
				row.Generation = &pub.Generation
			}
			for i, name := range pub.NamesOnDisk() {
				row.Renditions = append(row.Renditions, projectedRendition{Name: name, W: pub.Dims[i].W, H: pub.Dims[i].H})
			}
			out = append(out, row)
		}
	}
	return out
}

func (r *Registry) publicImages(item Item, m *Manifest) []PublicImage {
	var images []PublicImage
	for _, row := range item.Kind().publications(m) {
		image := PublicImage{From: row.Path, Preset: row.Preset}
		for _, rend := range row.Renditions {
			image.Renditions = append(image.Renditions, PublicRendition{URL: r.PublicURL(item.Ref(), rend.Name), W: rend.W, H: rend.H})
		}
		images = append(images, image)
	}
	return images
}

// projection is an item's publications as a journal outcome writes them.
type projection struct {
	ref  contentref.ContentRef
	rows []publication
}

// projectionOf projects m, the item's authoritative manifest (nil: no
// projection). A deleted or hidden item projects nothing.
func projectionOf(item Item, m *Manifest) *projection {
	if m == nil {
		return nil
	}
	return &projection{ref: item.Ref(), rows: item.Kind().publications(m)}
}

// without drops the publications with a file among retired keys.
func (p *projection) without(item Item, retired []string) *projection {
	if p == nil || len(retired) == 0 {
		return p
	}
	rows := slices.DeleteFunc(slices.Clone(p.rows), func(row publication) bool {
		return slices.ContainsFunc(row.Renditions, func(r projectedRendition) bool {
			return slices.Contains(retired, item.PublicPrefix()+r.Name)
		})
	})
	return &projection{ref: p.ref, rows: rows}
}

// project writes p in its own transaction.
func (j *PGJournal) project(ctx context.Context, p *projection) error {
	if p == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error { return j.projectTx(ctx, tx, p) })
}

// projectTx replaces the item's projected publications in tx, leaving
// unchanged rows untouched.
func (j *PGJournal) projectTx(ctx context.Context, tx pgx.Tx, p *projection) error {
	if p == nil {
		return nil
	}
	rows := p.rows
	if rows == nil {
		rows = []publication{}
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	t := j.publications
	_, err = tx.Exec(ctx, `WITH r AS (SELECT * FROM jsonb_to_recordset($4::jsonb)
AS r(name text, path text, preset text, ordinal integer, generation uuid, renditions jsonb)),
gone AS (DELETE FROM `+t+` p WHERE p.tenant_id = $1 AND p.content_kind = $2 AND p.content_id = $3
AND NOT EXISTS (SELECT 1 FROM r WHERE r.name = p.upload_name AND r.path = p.upload_path AND r.preset = p.preset))
INSERT INTO `+t+` AS p (tenant_id, content_kind, content_id, upload_name, upload_path, preset, ordinal, generation, renditions)
SELECT $1, $2, $3, r.name, r.path, r.preset, r.ordinal, r.generation, r.renditions FROM r
ON CONFLICT (tenant_id, content_kind, content_id, upload_name, upload_path, preset) DO UPDATE
SET ordinal = EXCLUDED.ordinal, generation = EXCLUDED.generation, renditions = EXCLUDED.renditions, updated_at = now()
WHERE (p.ordinal, p.generation, p.renditions) IS DISTINCT FROM (EXCLUDED.ordinal, EXCLUDED.generation, EXCLUDED.renditions)`,
		p.ref.TenantID, p.ref.ContentKind, p.ref.ContentID, body)
	return err
}
