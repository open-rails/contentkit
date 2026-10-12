package versions

import (
	"context"
	"time"
)

// segment is one of a version's segments (content_segments): an ordered
// page range with its own release time; none releases with its version.
type segment struct {
	Position  int        `json:"position"`
	Title     string     `json:"title,omitempty"`
	Start     int        `json:"start"`
	End       int        `json:"end"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
}

// segments loads each version's segments in page order.
func (s *Service) segments(ctx context.Context, q querier, versions []string) (map[string][]segment, error) {
	rows, err := q.Query(ctx, `SELECT version_id, position, title, start_index, end_index, release_at FROM `+s.t.segments+`
WHERE tenant_id = $1 AND version_id = ANY($2) ORDER BY version_id, start_index, position`, s.o.Tenant, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]segment{}
	for rows.Next() {
		var v string
		var g segment
		if err := rows.Scan(&v, &g.Position, &g.Title, &g.Start, &g.End, &g.ReleaseAt); err != nil {
			return nil, err
		}
		out[v] = append(out[v], g)
	}
	return out, rows.Err()
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
