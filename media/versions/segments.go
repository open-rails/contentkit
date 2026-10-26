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
	ID         string     `json:"id,omitempty"`
	Position   int        `json:"position"`
	Title      string     `json:"title,omitempty"`
	StartPage  string     `json:"start_page,omitempty"`
	Start      int        `json:"start"`
	End        int        `json:"end"`
	ReleaseAt  *time.Time `json:"release_at,omitempty"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

func legacyPageRanges(segs []segment) bool {
	return slices.ContainsFunc(segs, func(g segment) bool { return g.StartPage == "" })
}

// segments loads each version's segments in page order.
func (s *Service) segments(ctx context.Context, q querier, versions []string) (map[string][]segment, error) {
	rows, err := q.Query(ctx, `SELECT version_id, position, title, coalesce(start_index, 0), coalesce(end_index, 0), release_at, coalesce(start_page, ''),id,released_at FROM `+s.t.segments+`
WHERE tenant_id = $1 AND version_id = ANY($2) ORDER BY version_id, coalesce(start_index, position), position`, s.o.Tenant, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]segment{}
	for rows.Next() {
		var v string
		var g segment
		if err := rows.Scan(&v, &g.Position, &g.Title, &g.Start, &g.End, &g.ReleaseAt, &g.StartPage, &g.ID, &g.ReleasedAt); err != nil {
			return nil, err
		}
		out[v] = append(out[v], g)
	}
	return out, rows.Err()
}

// resolveChapterStarts derives ranges from current source-file order. Chapter
// titles, schedules and IDs travel with their markers when files are reordered.
func resolveChapterStarts(segs []segment, pages []page) ([]segment, error) {
	if !slices.ContainsFunc(segs, func(s segment) bool { return s.StartPage != "" }) {
		return segs, nil
	}
	positions := make(map[string][]int, len(pages))
	for i, page := range pages {
		id := page.Path
		if id != "" {
			positions[id] = append(positions[id], i)
		}
	}
	out := slices.Clone(segs)
	for i := range out {
		g := &out[i]
		matches := positions[g.StartPage]
		if g.StartPage == "" || len(matches) == 0 {
			return nil, fmt.Errorf("%w: chapter %d's start page is missing; choose another page", ErrInvalid, g.Position+1)
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("%w: chapter %d's start page appears more than once; choose another page", ErrInvalid, g.Position+1)
		}
		g.Start, g.End = matches[0], len(pages)
	}
	slices.SortFunc(out, func(a, b segment) int { return a.Start - b.Start })
	for i := range out {
		out[i].Position = i
		if i > 0 {
			if out[i-1].Start == out[i].Start {
				return nil, fmt.Errorf("%w: chapter start %q is repeated", ErrInvalid, out[i].StartPage)
			}
			out[i-1].End = out[i].Start
		}
	}
	return out, nil
}

// readableStarts moves a marker whose source page is excluded from reading to
// the next included page, so every included page keeps its chapter. A moved
// chapter left with no included page is dropped; other markers pass through.
func readableStarts(segs []segment, source []media.File) []segment {
	at := make(map[string]int, len(source))
	next := make([]string, len(source))
	following := ""
	for i := len(source) - 1; i >= 0; i-- {
		at[source[i].Path] = i
		if media.PageIncluded(source[i]) {
			following = source[i].Path
		}
		next[i] = following
	}
	from := make([]int, len(segs))
	owner := map[string]int{}
	out := slices.Clone(segs)
	for i := range out {
		g := &out[i]
		from[i] = -1
		if index, ok := at[g.StartPage]; ok {
			from[i], g.StartPage = index, next[index]
			if prior, taken := owner[g.StartPage]; !taken || from[prior] < index {
				owner[g.StartPage] = i
			}
		}
	}
	keep := out[:0]
	for i, g := range out {
		if from[i] < 0 || g.StartPage == segs[i].StartPage || g.StartPage != "" && owner[g.StartPage] == i {
			keep = append(keep, g)
		}
	}
	return keep
}

// standIns maps each included page showing an excluded page's content (the
// same bytes or pixels, or an overlapping page range where either is a
// spread) to the excluded page's chapter, by source order. It also waits for
// that chapter's date.
func standIns(segs []segment, source []media.File) map[string][]segment {
	at := make(map[string]int, len(source))
	for i, f := range source {
		at[f.Path] = i
	}
	starts := map[int]segment{}
	for _, g := range segs {
		if i, ok := at[g.StartPage]; ok {
			starts[i] = g
		}
	}
	keys := make([][]string, len(source))
	spans := make([]*PageSpan, len(source))
	for i, f := range source {
		meta := pageMeta(f)
		if digest := pageDigest(f); digest != "" {
			keys[i] = append(keys[i], "bytes:"+digest)
			if a := meta.Analysis; a != nil && a.Recipe == media.PageAnalysisRecipe && a.Status == "complete" && a.SourceSHA256 == digest && a.PixelSHA256 != "" {
				keys[i] = append(keys[i], "pixels:"+a.PixelSHA256)
			}
		}
		spans[i] = meta.Span
	}
	shown := map[string][]string{}
	var spanned []int
	for i, f := range source {
		if !media.PageIncluded(f) {
			continue
		}
		for _, key := range keys[i] {
			shown[key] = append(shown[key], f.Path)
		}
		if spans[i] != nil {
			spanned = append(spanned, i)
		}
	}
	holds := map[string][]segment{}
	var chapter *segment
	for i, f := range source {
		if g, ok := starts[i]; ok {
			chapter = &g
		}
		if media.PageIncluded(f) || chapter == nil {
			continue
		}
		held := map[string]bool{}
		for _, key := range keys[i] {
			for _, path := range shown[key] {
				held[path] = true
			}
		}
		if span := spans[i]; span != nil {
			for _, j := range spanned {
				over := spans[j]
				if (span.From < span.To || over.From < over.To) && over.From <= span.To && span.From <= over.To {
					held[source[j].Path] = true
				}
			}
		}
		for path := range held {
			holds[path] = append(holds[path], *chapter)
		}
	}
	return holds
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
	ref, err := s.ref(k.Name, target)
	if err != nil {
		return err
	}
	m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
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
