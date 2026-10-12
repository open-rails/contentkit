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
	"github.com/open-rails/contentkit/media"
)

// ReaderPlan is how a viewer reads a version: its current publication's
// segments, each with its release state and, when the viewer opens it, the
// published item to read through the media read API; the public previews
// of a locked version; the playback item; and the reader metadata naming
// what the viewer can read. It signs nothing.
type ReaderPlan struct {
	Content     string        `json:"content_id"`
	Version     string        `json:"version_id"`
	Publication string        `json:"publication_id"`
	Pages       int           `json:"pages"`
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

// decision is one viewer's verdict on a span of a version's pages.
type decision struct {
	sight Sight
	open  Opening
	segs  []segment
	pages int
	now   time.Time
}

// shows reports pages [start, end) shown: released, or to an editor.
func (d decision) shows(end int) bool {
	return d.sight.Visible && (d.sight.Editor || end <= released(d.segs, d.pages, d.now))
}

// opens reports pages [start, end) open: the version opens, or they lie in
// its free segments.
func (d decision) opens(end int) bool {
	return d.open.Opens || d.sight.Editor || end <= freeEnd(d.segs, d.open.Free, d.pages)
}

// playback reports the version's audio open: every segment released and
// the version open, or to an editor.
func (d decision) playback() bool {
	return d.sight.Visible && (d.sight.Editor || d.open.Opens && released(d.segs, math.MaxInt, d.now) == math.MaxInt)
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
	sights, err := s.o.Visibility.See(ctx, actor, []Target{t})
	if err != nil {
		return nil, err
	}
	if !sights[t].Visible {
		return nil, ErrNotFound
	}
	opens, err := s.o.Access.Open(ctx, actor, []Target{t})
	if err != nil {
		return nil, err
	}
	p, err := s.current(ctx, s.o.Pool, t, kindPages)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrNotReady
	}
	var snap pagesSnapshot
	if err := json.Unmarshal(p.Source, &snap); err != nil {
		return nil, err
	}
	segs, err := s.segments(ctx, s.o.Pool, []string{t.Version})
	if err != nil {
		return nil, err
	}
	d := decision{sight: sights[t], open: opens[t], segs: segs[t.Version], pages: len(snap.Pages), now: s.o.Now()}
	cut := released(d.segs, d.pages, d.now)
	plan := &ReaderPlan{Content: t.Content, Version: t.Version, Publication: p.ID, Pages: len(snap.Pages), Segments: []PlanSegment{},
		Previews: []PlanPreview{}}
	byPos := map[int]segment{}
	for _, g := range d.segs {
		byPos[g.Position] = g
	}
	readable := map[string]bool{}
	for _, it := range p.Items {
		seg := PlanSegment{Position: it.Segment, Start: it.Start, End: it.End, Released: d.sight.Live && it.End <= cut,
			Locked: !d.shows(it.End) || !d.opens(it.End)}
		if it.Segment != nil {
			seg.Title, seg.ReleaseAt = byPos[*it.Segment].Title, byPos[*it.Segment].ReleaseAt
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
			if err != nil || n < 1 || n > min(cut, d.pages) || len(im.Renditions) == 0 {
				continue
			}
			pg := snap.Pages[n-1]
			r := im.Renditions[len(im.Renditions)-1]
			plan.Previews = append(plan.Previews, PlanPreview{Index: n - 1, Name: k.NameOf(pg.Path), URL: r.URL, W: r.W, H: r.H})
			readable[k.NameOf(pg.Path)] = true
		}
	}
	files := map[string]bool{}
	if k.Publish.Playback != "" && d.playback() {
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
