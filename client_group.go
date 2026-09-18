package contentkit

import "sort"

// groupedDoc is one scored document with its retrieval provenance.
type groupedDoc struct {
	ref      ContentRef
	language string
	priority int32
	score    float32
	// requested is true when the document language is the request language,
	// not a fallback.
	requested     bool
	scoreKind     ScoreKind
	contributions []ContributionTrace
}

// group is one content item: ranked by its best document in any searched
// language, represented by the best document in the requested language when
// one matched.
type group struct {
	best           groupedDoc
	representative groupedDoc
}

// rankLess orders documents for item ranking: score, requested language,
// language, content kind, id, version. It agrees with each source's own order,
// so a window prefix of documents yields a prefix of items.
func rankLess(a, b groupedDoc) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.requested != b.requested {
		return a.requested
	}
	if a.language != b.language {
		return a.language < b.language
	}
	if a.ref.ContentKind != b.ref.ContentKind {
		return a.ref.ContentKind < b.ref.ContentKind
	}
	if a.ref.ContentID != b.ref.ContentID {
		return a.ref.ContentID < b.ref.ContentID
	}
	return a.ref.Version() < b.ref.Version()
}

// representLess chooses the document returned for an item: the requested
// language first, then score, host priority, language and version.
func representLess(a, b groupedDoc) bool {
	if a.requested != b.requested {
		return a.requested
	}
	if a.score != b.score {
		return a.score > b.score
	}
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	if a.language != b.language {
		return a.language < b.language
	}
	return a.ref.Version() < b.ref.Version()
}

// groupByContent keeps one entry per work (tenant, kind, content id) ordered by
// rankLess of each item's best document. It runs before any page limit.
func groupByContent(docs []groupedDoc) []group {
	index := map[ContentKey]int{}
	groups := make([]group, 0, len(docs))
	for _, d := range docs {
		k := d.ref.Content().Key()
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
