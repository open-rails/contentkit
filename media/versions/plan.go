package versions

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"slices"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// ReaderPlan is a ready version snapshot with live chapter metadata. Released
// spans select existing files in the same canonical version; their URLs are
// signed on demand. Locked previews sign only their permitted renditions.
type ReaderPlan struct {
	NextReleaseAt *time.Time    `json:"next_release_at,omitempty"`
	Content       string        `json:"content_id"`
	Version       string        `json:"version_id"`
	Publication   string        `json:"publication_id"`
	Pages         *int          `json:"pages,omitempty"`
	Segments      []PlanSegment `json:"segments"`
	Previews      []PlanPreview `json:"previews"`
	Playback      *PlanPlayback `json:"playback,omitempty"`
	Reader        *ReaderMeta   `json:"reader,omitempty"`
	Expires       int64         `json:"expires,omitempty"` // signed preview URL expiry, Unix seconds
}

// PlanSegment is one span of pages: a segment's (Position set) or pages
// outside every segment.
type PlanSegment struct {
	Position   *int           `json:"position,omitempty"`
	Title      string         `json:"title,omitempty"`
	Start      int            `json:"start"`
	End        int            `json:"end"`
	Released   bool           `json:"released"`
	ReleaseAt  *time.Time     `json:"release_at,omitempty"`
	Locked     bool           `json:"locked"`
	Inspection bool           `json:"inspection,omitempty"` // authorized prepared derivatives; fetch with explicit management read
	Item       *media.RefBody `json:"item,omitempty"`       // read it at GET /media/{kind}/{id}
	Pages      []PlanPage     `json:"pages,omitempty"`
}

// PlanPage is one page of an open segment: its name, reading size and the
// path of each rendition in the segment's item.
type PlanPage struct {
	ID    string            `json:"id"` // original image SHA-256; the canonical page identity
	Name  string            `json:"name"`
	Span  *PageSpan         `json:"span,omitempty"`
	W     int               `json:"w"`
	H     int               `json:"h"`
	Files map[string]string `json:"files"`
}

// PlanPreview is a public preview of a leading page.
type PlanPreview struct {
	ID    string `json:"id"` // original image SHA-256
	Index int    `json:"index"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// PlanPlayback is the version's audio item and its tracks.
type PlanPlayback struct {
	Item   media.RefBody `json:"item"`
	Tracks []PlanTrack   `json:"tracks"`
}

type PlanTrack struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Duration float64 `json:"duration,omitempty"`
}

// decision is one viewer's verdict on a span of a version's pages: the
// resolver's on the version, its segments' release and its rule's preview.
type decision struct {
	res      resolve.Resolution
	preview  access.Preview
	segs     []segment
	pages    int
	anchored bool
	now      time.Time
}

func previewPageCount(k *media.Kind, d decision, total int) int {
	if k.Publish.Previews == "" {
		return 0
	}
	spec := public(k, k.Publish.Previews)
	if spec == nil {
		return 0
	}
	return max(0, min(total, d.preview.Pages, spec.First))
}

// shows reports released pages, or all pages to an authorized restricted reader.
func (d decision) shows(end int) bool {
	return d.res.Visible
}

// opens reports pages [start, end) open: the version opens, or they lie in
// its rule's free segments.
func (d decision) opens(end int) bool {
	return d.res.Accessible || d.res.Editor || d.res.ReadRestricted
}

// whole reports the whole version open: every segment released and the
// version open, or to an editor. Its audio and downloads need it.
func (d decision) whole() bool {
	return d.res.Visible && (d.res.Editor || d.res.Accessible)
}

// wholeForRead includes explicit restricted reading without changing the
// download policy or its editor/watermark distinction.
func (d decision) wholeForRead() bool {
	return d.res.Visible && (d.res.ReadRestricted || d.whole())
}

// decisions use completed publication metadata. Chapter dates select work;
// they never independently authorize or expose prepared media.
func (s *Service) decisions(ctx context.Context, actor access.Actor, kind string, vs []Target, snapshots map[Target]pagesSnapshot) (map[Target]decision, error) {
	res, err := s.decide(ctx, actor, kind, vs)
	if err != nil {
		return nil, err
	}
	out := map[Target]decision{}
	for _, v := range vs {
		snap, ok := snapshots[v]
		if !ok {
			p, err := s.current(ctx, s.o.Pool, v, kindPages)
			if err != nil {
				return nil, err
			}
			if p != nil {
				if err := json.Unmarshal(p.Source, &snap); err != nil {
					return nil, err
				}
			}
		}
		out[v] = decision{res: res[v], segs: snap.Segments, pages: len(snap.Pages), anchored: true, now: s.o.Now()}
	}
	return out, nil
}

// Plan derives chapter spans from current metadata over an immutable file
// snapshot. Every span reads the same canonical version item.
func (s *Service) Plan(ctx context.Context, actor access.Actor, t Target) (*ReaderPlan, error) {
	v, err := s.one(ctx, t)
	if err != nil {
		return nil, err
	}
	k, err := s.published(v)
	if err != nil {
		return nil, err
	}
	resolved, err := s.decide(ctx, actor, v.Kind, []Target{t})
	if err != nil {
		return nil, err
	}
	if verdict := resolved[t]; verdict.Visible && verdict.ReadRestricted && !verdict.Editor {
		return s.inspectionPlan(ctx, actor, k, t)
	}
	p, err := s.current(ctx, s.o.Pool, t, kindPages)
	if err != nil {
		return nil, err
	}
	var snap pagesSnapshot
	snapshots := map[Target]pagesSnapshot{}
	if p != nil {
		if err := json.Unmarshal(p.Source, &snap); err != nil {
			return nil, err
		}
		snapshots[t] = snap
	}
	ds, err := s.decisions(ctx, actor, v.Kind, []Target{t}, snapshots)
	if err != nil {
		return nil, err
	}
	d := ds[t]
	if !d.res.Visible {
		return nil, ErrNotFound
	}
	if p == nil {
		return nil, ErrNotReady
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !m.PublicationLayoutReady() {
		return nil, ErrNotReady
	}
	if active, err := s.activeRelease(ctx, s.o.Pool, t); err != nil {
		return nil, err
	} else if active != nil && active.View != nil {
		m = active.View
	} else {
		return nil, ErrNotReady
	}
	paths := map[string]string{}
	for _, f := range m.Files {
		if f.Blob != "" && !f.Gone {
			paths[f.Blob] = f.Path
		}
	}
	// Paths are mutable display metadata; a snapshot grants only its exact
	// immutable blobs. A renamed file still resolves without broadening access.
	for i := range snap.Pages {
		for j := range snap.Pages[i].Files {
			r := &snap.Pages[i].Files[j]
			if path, ok := paths[r.Blob]; ok {
				r.Path = path
			} else {
				return nil, ErrNotReady
			}
		}
	}
	cut := len(snap.Pages)
	length := d.res.Accessible || d.res.Editor || d.res.ReadRestricted || s.o.Kinds.Kinds[v.Kind].LockedView().Shows(kinds.ShowLength)
	plan := &ReaderPlan{Content: t.Content, Version: t.Version, Publication: p.ID, Segments: []PlanSegment{}, Previews: []PlanPreview{}}
	if err := s.o.Pool.QueryRow(ctx, `SELECT scheduled_at FROM `+s.publicationRequestsTable()+`
 WHERE tenant_id=$1 AND version_id=$2 AND current AND state IN ('pending','waiting','running','failed')`, s.o.Tenant, t.Version).Scan(&plan.NextReleaseAt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if length {
		n := len(snap.Pages)
		plan.Pages = &n
	}
	byPos := map[int]segment{}
	for _, g := range d.segs {
		byPos[g.Position] = g
	}
	item := &media.RefBody{Kind: ref.ContentKind, ID: ref.ContentID, Version: ref.Version()}
	readable := map[string]bool{}
	for _, span := range spans(d.segs, len(snap.Pages)) {
		seg := PlanSegment{Position: span.Segment, Start: span.Start, End: span.End, Released: span.End <= cut, Locked: !d.shows(span.End) || !d.opens(span.End)}
		if span.Segment != nil {
			g := byPos[*span.Segment]
			seg.Title, seg.ReleaseAt = g.Title, g.ReleaseAt
		}
		if seg.Locked && !length {
			continue
		}
		if !seg.Locked {
			seg.Item = item
			for _, pg := range snap.Pages[span.Start:span.End] {
				name := pg.Name
				if name == "" {
					name = k.NameOf(pg.Path)
				}
				pp := PlanPage{ID: sourcePageHash(pg), Name: name, Span: pg.Span, W: pg.Files[0].W, H: pg.Files[0].H, Files: map[string]string{}}
				for _, r := range pg.Files {
					pp.Files[r.Preset] = r.Path
				}
				readable[pp.ID] = true
				seg.Pages = append(seg.Pages, pp)
			}
		}
		plan.Segments = append(plan.Segments, seg)
	}
	if k.Publish.Previews != "" {
		count := previewPageCount(k, d, len(snap.Pages))
		requested := make([]string, 0, count)
		for _, pg := range snap.Pages[:count] {
			if len(pg.Files) > 0 {
				requested = append(requested, pg.Files[0].Path)
			}
		}
		urls := map[string]string{}
		for batch := range slices.Chunk(requested, 64) {
			read, err := s.m.Reader.Read(ctx, ref, actor, media.ReadOptions{Paths: batch})
			if err != nil {
				return nil, err
			}
			if plan.Expires == 0 || read.Expires < plan.Expires {
				plan.Expires = read.Expires
			}
			for _, f := range read.Files {
				if f.URL != "" {
					urls[f.Path] = f.URL
				}
			}
		}
		for i, pg := range snap.Pages[:count] {
			if len(pg.Files) == 0 {
				continue
			}
			r := pg.Files[0]
			url := urls[r.Path]
			if url == "" {
				continue
			}
			name := pg.Name
			if name == "" {
				name = k.NameOf(pg.Path)
			}
			plan.Previews = append(plan.Previews, PlanPreview{ID: sourcePageHash(pg), Index: i, Name: name, URL: url, W: r.W, H: r.H})
			readable[sourcePageHash(pg)] = true
		}
	}
	files := map[string]bool{}
	if k.Publish.Playback != "" && d.wholeForRead() {
		pb, err := s.current(ctx, s.o.Pool, t, kindPlayback)
		if err != nil {
			return nil, err
		}
		if pb != nil {
			var tracks playbackSnapshot
			if err := json.Unmarshal(pb.Source, &tracks); err != nil {
				return nil, err
			}
			if len(tracks.Tracks) > 0 {
				plan.Playback = &PlanPlayback{Item: *item}
				for _, tr := range tracks.Tracks {
					path, ok := paths[tr.File.Blob]
					if !ok {
						return nil, ErrNotReady
					}
					plan.Playback.Tracks = append(plan.Playback.Tracks, PlanTrack{Name: tr.Name, Path: path, Duration: tr.File.Dur})
					files[tr.Name] = true
				}
			}
		}
	}
	if rm, ok := readerMetaOf(m); ok {
		kept := rm.hashPages(snap.Pages).filter(readable, files)
		plan.Reader = &kept
	}
	return plan, nil
}
