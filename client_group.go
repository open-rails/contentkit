package searchkit

import (
	"sort"

	"github.com/open-rails/searchkit/search"
)

// Eligibility is the host's per-document eligibility join; see search.Eligibility.
type Eligibility = search.Eligibility

// groupedDoc is one scored document with its retrieval provenance.
type groupedDoc struct {
	EntityType string
	EntityID   string
	ParentID   string
	Language   string
	Priority   int32
	Score      float32
	// requested is true when the document language is the request language,
	// not a fallback.
	requested   bool
	sourceIndex int
	sourceRank  int
	// fused indexes the RRF-fused list in dual/semantic modes.
	fused int
}

// group is one content item: ranked by its best document in any searched
// language, represented by the best document in the requested language when
// one matched.
type group struct {
	best           groupedDoc
	representative groupedDoc
}

type groupKey struct{ entityType, parentID string }

// rankLess orders documents for item ranking: score, requested language,
// language, entity type, entity id. It agrees with each source's own order, so
// a window prefix of documents yields a prefix of items.
func rankLess(a, b groupedDoc) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.requested != b.requested {
		return a.requested
	}
	if a.Language != b.Language {
		return a.Language < b.Language
	}
	if a.EntityType != b.EntityType {
		return a.EntityType < b.EntityType
	}
	return a.EntityID < b.EntityID
}

// representLess chooses the document returned for an item: the requested
// language first, then score, host priority, language and entity id.
func representLess(a, b groupedDoc) bool {
	if a.requested != b.requested {
		return a.requested
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if a.Language != b.Language {
		return a.Language < b.Language
	}
	return a.EntityID < b.EntityID
}

// groupByParent keeps one entry per (entity type, parent) ordered by rankLess
// of each item's best document. It runs before any page limit.
func groupByParent(docs []groupedDoc) []group {
	index := map[groupKey]int{}
	groups := make([]group, 0, len(docs))
	for _, d := range docs {
		if d.ParentID == "" {
			d.ParentID = d.EntityID
		}
		k := groupKey{d.EntityType, d.ParentID}
		i, ok := index[k]
		if !ok {
			index[k] = len(groups)
			groups = append(groups, group{best: d, representative: d})
			continue
		}
		if rankLess(d, groups[i].best) {
			groups[i].best = d
		}
		if representLess(d, groups[i].representative) {
			groups[i].representative = d
		}
	}
	sort.Slice(groups, func(i, j int) bool { return rankLess(groups[i].best, groups[j].best) })
	return groups
}

// page slices groups by offset/limit and reports whether items follow.
func page(groups []group, offset, limit int) ([]group, bool) {
	if offset >= len(groups) {
		return nil, false
	}
	end := offset + limit
	hasMore := end < len(groups)
	if end > len(groups) {
		end = len(groups)
	}
	return groups[offset:end], hasMore
}
