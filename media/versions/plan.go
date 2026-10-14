package versions

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// ReaderPlan is how a viewer reads a version: its current publication's
// segments, each with its release state and, when the viewer opens it, the
// published item to read through the media read API; the public previews
// of a locked version; the playback item; and the reader metadata naming
// what the viewer can read. It signs nothing. A viewer locked out of a
// version whose kind's locked view hides its length gets neither its page
// count (absent) nor its locked segments.
type ReaderPlan struct {
	Content     string        `json:"content_id"`
	Version     string        `json:"version_id"`
	Publication string        `json:"publication_id"`
	Pages       *int          `json:"pages,omitempty"`
	Segments    []PlanSegment `json:"segments"`
	Previews    []PlanPreview `json:"previews"`
	Playback    *PlanPlayback `json:"playback,omitempty"`
	Reader      *ReaderMeta   `json:"reader,omitempty"`
}

// PlanSegment is one span of pages: a segment's (Position set) or pages
// outside every segment.
type PlanSegment struct {
	Position  *int           `json:"position,omitempty"`
	Title     string         `json:"title,omitempty"`
	Start     int            `json:"start"`
	End       int            `json:"end"`
	Released  bool           `json:"released"`
	ReleaseAt *time.Time     `json:"release_at,omitempty"`
	Locked    bool           `json:"locked"`
	Item      *media.RefBody `json:"item,omitempty"` // read it at GET /media/{kind}/{id}
	Pages     []PlanPage     `json:"pages,omitempty"`
}

// PlanPage is one page of an open segment: its name, reading size and the
// path of each rendition in the segment's item.
type PlanPage struct {
	Name  string            `json:"name"`
	W     int               `json:"w"`
	H     int               `json:"h"`
	Files map[string]string `json:"files"`
}

// PlanPreview is a public preview of a leading page.
type PlanPreview struct {
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
	res     resolve.Resolution
	preview access.Preview
	segs    []segment
	pages   int
	now     time.Time
}

// shows reports pages [start, end) shown: released, or to an editor.
func (d decision) shows(end int) bool {
	return d.res.Visible && (d.res.Editor || end <= released(d.segs, d.pages, d.now))
}

// opens reports pages [start, end) open: the version opens, or they lie in
// its rule's free segments.
func (d decision) opens(end int) bool {
	return d.res.Accessible || d.res.Editor || end <= freeEnd(d.segs, d.preview.Segments, d.pages)
}

// whole reports the whole version open: every segment released and the
// version open, or to an editor. Its audio and downloads need it.
func (d decision) whole() bool {
	return d.res.Visible && (d.res.Editor || d.res.Accessible && released(d.segs, math.MaxInt, d.now) == math.MaxInt)
}

// decisions are actor's decisions on versions of kind, each over pages.
func (s *Service) decisions(ctx context.Context, actor access.Actor, kind string, vs []Target, pages map[Target]int) (map[Target]decision, error) {
	res, err := s.decide(ctx, actor, kind, vs)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(vs))
	for i, v := range vs {
		ids[i] = v.Version
	}
	previews, err := s.previews(ctx, s.o.Pool, ids)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments(ctx, s.o.Pool, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[Target]decision, len(vs))
	for _, v := range vs {
		out[v] = decision{res: res[v], preview: previews[v.Version], segs: segs[v.Version], pages: pages[v], now: s.o.Now()}
	}
	return out, nil
}

// Plan is t's reader plan for actor.
func (s *Service) Plan(ctx context.Context, actor access.Actor, t Target) (*ReaderPlan, error) {
	v, err := s.one(ctx, t)
	if err != nil {
		return nil, err
	}
	k, err := s.published(v)
	if err != nil {
		return nil, err
	}
	p, err := s.current(ctx, s.o.Pool, t, kindPages)
	if err != nil {
		return nil, err
	}
	var snap pagesSnapshot
	if p != nil {
		if err := json.Unmarshal(p.Source, &snap); err != nil {
			return nil, err
		}
	}
	ds, err := s.decisions(ctx, actor, v.Kind, []Target{t}, map[Target]int{t: len(snap.Pages)})
	if err != nil {
		return nil, err
	}
	d := ds[t]
	switch {
	case !d.res.Visible:
		return nil, ErrNotFound
	case p == nil:
		return nil, ErrNotReady
	}
	cut := released(d.segs, d.pages, d.now)
	length := d.res.Accessible || d.res.Editor || s.o.Kinds.Kinds[v.Kind].LockedView().Shows(kinds.ShowLength)
	plan := &ReaderPlan{Content: t.Content, Version: t.Version, Publication: p.ID, Segments: []PlanSegment{}, Previews: []PlanPreview{}}
	if length {
		n := len(snap.Pages)
		plan.Pages = &n
	}
	byPos := map[int]segment{}
	for _, g := range d.segs {
		byPos[g.Position] = g
	}
	readable := map[string]bool{}
	for _, it := range p.Items {
		seg := PlanSegment{Position: it.Segment, Start: it.Start, End: it.End, Released: it.End <= cut,
			Locked: !d.shows(it.End) || !d.opens(it.End)}
		if it.Segment != nil {
			seg.Title, seg.ReleaseAt = byPos[*it.Segment].Title, byPos[*it.Segment].ReleaseAt
		}
		if seg.Locked && !length {
			continue
		}
		if !seg.Locked {
			ref := s.publishedRef(k, it.ID)
			seg.Item = &media.RefBody{Kind: ref.ContentKind, ID: ref.ContentID}
			for _, pg := range snap.Pages[it.Start:it.End] {
				pp := PlanPage{Name: k.NameOf(pg.Path), W: pg.Files[0].W, H: pg.Files[0].H, Files: map[string]string{}}
				for _, r := range pg.Files {
					pp.Files[r.Preset] = r.Path
				}
				readable[pp.Name] = true
				seg.Pages = append(seg.Pages, pp)
			}
		}
		plan.Segments = append(plan.Segments, seg)
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return nil, err
	}
	if k.Publish.Previews != "" {
		images, err := s.m.Manifests.Images(ctx, media.ImageQuery{Ref: ref, Preset: k.Publish.Previews})
		if err != nil {
			return nil, err
		}
		dir := previewDir(k)
		for _, im := range images[0] {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(im.From, dir), ".webp"))
			if err != nil || n < 1 || n > min(cut, d.pages, d.preview.Pages) || len(im.Renditions) == 0 {
				continue
			}
			pg := snap.Pages[n-1]
			r := im.Renditions[len(im.Renditions)-1]
			plan.Previews = append(plan.Previews, PlanPreview{Index: n - 1, Name: k.NameOf(pg.Path), URL: r.URL, W: r.W, H: r.H})
			readable[k.NameOf(pg.Path)] = true
		}
	}
	files := map[string]bool{}
	if k.Publish.Playback != "" && d.whole() {
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
				item := s.publishedRef(k, pb.ID)
				plan.Playback = &PlanPlayback{Item: media.RefBody{Kind: item.ContentKind, ID: item.ContentID}}
				for _, tr := range tracks.Tracks {
					plan.Playback.Tracks = append(plan.Playback.Tracks, PlanTrack{Name: tr.Name, Path: tr.File.Path, Duration: tr.File.Dur})
					files[tr.Name] = true
				}
			}
		}
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return nil, err
	}
	if m != nil {
		if rm, ok := readerMetaOf(m); ok {
			kept := rm.filter(readable, files)
			plan.Reader = &kept
		}
	}
	return plan, nil
}
