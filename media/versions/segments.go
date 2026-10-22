package versions

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// segment is one of a version's segments (content_segments): an ordered
// page range with its own release time; none releases with its version.
type segment struct {
	Position  int        `json:"position"`
	Title     string     `json:"title,omitempty"`
	StartPage string     `json:"start_page,omitempty"`
	Start     int        `json:"start"`
	End       int        `json:"end"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
}

func legacyPageRanges(segs []segment) bool {
	return slices.ContainsFunc(segs, func(g segment) bool { return g.StartPage == "" })
}

// segments loads each version's segments in page order.
func (s *Service) segments(ctx context.Context, q querier, versions []string) (map[string][]segment, error) {
	rows, err := q.Query(ctx, `SELECT version_id, position, title, coalesce(start_index, 0), coalesce(end_index, 0), release_at, coalesce(start_page, '') FROM `+s.t.segments+`
WHERE tenant_id = $1 AND version_id = ANY($2) ORDER BY version_id, coalesce(start_index, position), position`, s.o.Tenant, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]segment{}
	for rows.Next() {
		var v string
		var g segment
		if err := rows.Scan(&v, &g.Position, &g.Title, &g.Start, &g.End, &g.ReleaseAt, &g.StartPage); err != nil {
			return nil, err
		}
		out[v] = append(out[v], g)
	}
	return out, rows.Err()
}

// resolveChapterStarts derives ranges against this exact source/publication
// snapshot. A missing or reordered marker requires repair; never retarget it.
func resolveChapterStarts(segs []segment, pages []page) ([]segment, error) {
	if !slices.ContainsFunc(segs, func(s segment) bool { return s.StartPage != "" }) {
		return segs, nil
	}
	positions := make(map[string][]int, len(pages))
	for i, page := range pages {
		id := sourcePageHash(page)
		if id != "" {
			positions[id] = append(positions[id], i)
		}
	}
	out := slices.Clone(segs)
	previous := -1
	for i := range out {
		g := &out[i]
		matches := positions[g.StartPage]
		if g.StartPage == "" || len(matches) == 0 {
			return nil, fmt.Errorf("%w: chapter %d's start page is missing; choose another page", ErrInvalid, g.Position+1)
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("%w: chapter %d's start page appears more than once; choose another page", ErrInvalid, g.Position+1)
		}
		at := matches[0]
		if at <= previous {
			return nil, fmt.Errorf("%w: chapter %d must start after the chapter before it", ErrInvalid, g.Position+1)
		}
		g.Start, g.End = at, len(pages)
		if i > 0 {
			out[i-1].End = at
		}
		previous = at
	}
	return out, nil
}

func sourcePageHash(p page) string {
	if p.SourceHash != "" {
		return p.SourceHash
	}
	// Older snapshots already record the original blob and edit. Never use
	// their filename or any derived rendition as a replacement identity.
	blob, _, _ := strings.Cut(p.Source, ".")
	if digest, ok := layout.BlobDigest(blob); ok {
		return hex.EncodeToString(digest)
	}
	return ""
}

// ValidateChapterStartsTx checks authored starts against the current source
// manifest, including drafts whose renditions have not finished. The catalog
// caller holds the version's source lock before taking its row locks.
func (s *Service) ValidateChapterStartsTx(ctx context.Context, tx pgx.Tx, target Target, starts []string) error {
	versions, err := s.versions(ctx, tx, []Target{target})
	if err != nil {
		return err
	}
	v, ok := versions[target]
	if !ok {
		return ErrNotFound
	}
	k, err := s.published(v)
	if err != nil {
		return err
	}
	m, err := s.manifest(ctx, k, target)
	if err != nil {
		return err
	}
	var pages []page
	for _, f := range sourcePages(k, m) {
		if media.PageIncluded(f) {
			pages = append(pages, page{SourceHash: k.PageID(f), Path: f.Path})
		}
	}
	segs := make([]segment, len(starts))
	for i, start := range starts {
		segs[i] = segment{Position: i, StartPage: start}
	}
	_, err = resolveChapterStarts(segs, pages)
	return err
}

// released is how many leading pages are released: up to the first
// unreleased segment's start.
func released(segs []segment, total int, now time.Time) int {
	for _, g := range segs {
		if g.ReleaseAt != nil && g.ReleaseAt.After(now) {
			return min(max(g.Start, 0), total)
		}
	}
	return total
}

// freeEnd is where the leading n segments' pages end.
func freeEnd(segs []segment, n, total int) int {
	if n <= 0 {
		return 0
	}
	if n > len(segs) {
		return total
	}
	return min(segs[n-1].End, total)
}

// span is one published item's pages: a segment's, or (Segment nil) pages
// outside every segment.
type span struct {
	Segment *int `json:"segment,omitempty"`
	Start   int  `json:"start"`
	End     int  `json:"end"`
}

// spans divides total pages into items: each segment's pages, clipped to
// the pages there are and to the segment before, and the pages between.
func spans(segs []segment, total int) []span {
	var out []span
	at := 0
	for _, g := range segs {
		start, end := max(g.Start, at), min(g.End, total)
		if start >= end {
			continue
		}
		if start > at {
			out = append(out, span{Start: at, End: start})
		}
		pos := g.Position
		out = append(out, span{Segment: &pos, Start: start, End: end})
		at = end
	}
	if at < total {
		out = append(out, span{Start: at, End: total})
	}
	return out
}
