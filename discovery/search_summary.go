package discovery

import (
	"context"

	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/search"
)

// Search summaries reuse keyword retrieval. They never count unrelated hidden
// galleries merely because those galleries share a catalog with this search.
// One relaxed, hard-policy-constrained candidate set is compared against both
// preference selections; a filled retrieval window is an explicit lower bound.
func (s *Service) searchSummary(ctx context.Context, o SearchOptions, selected *sel, sort order) (*ListingSummary, error) {
	reader := selected.reader
	relaxed, err := s.compileWithViewer(ctx, reader, o.Filter, selected.viewer.WithoutContentFilters())
	if err != nil {
		return nil, err
	}
	limit := search.MaxCandidateLimit
	if o.Sort != "" && o.Sort != SortRelevance {
		limit = sortedMatches
	}
	res, err := s.o.Hub.Search(ctx, o.Query, hub.HubSearchOptions{SearchOptions: hub.SearchOptions{
		Language: relaxed.display, LanguageMode: s.languageMode(), ContentKinds: s.searchKinds(o.Kind),
		Eligibility: s.eligibility(relaxed), Limit: limit, CandidateLimit: search.MaxCandidateLimit,
	}})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		ids = append(ids, hit.ContentID)
	}
	return s.listingSummary(ctx, o.Filter, selected, sort, &summaryCandidates{
		visible: ids, relaxed: ids, truncated: res.Truncated || res.HasMore,
	})
}
