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
	opts := hub.SearchOptions{Language: relaxed.display, LanguageMode: s.languageMode(), ContentKinds: s.searchKinds(o.Kind),
		Eligibility: s.eligibility(relaxed), Limit: search.MaxCandidateLimit, CandidateLimit: search.MaxCandidateLimit}
	match, err := s.o.Hub.KeywordMatch(ctx, o.Query, opts)
	if err != nil {
		return nil, err
	}
	matchingEditions := func(q *sel) *sel { return q.with("(f.content_id,f.version_id) IN ("+match.SQL+")", match.Args) }
	if o.Sort != "" && o.Sort != SortRelevance {
		return s.listingSummary(ctx, o.Filter, selected, sort, &summaryCandidates{selection: matchingEditions})
	}
	res, err := s.o.Hub.Search(ctx, o.Query, hub.HubSearchOptions{SearchOptions: opts})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		ids = append(ids, hit.ContentID)
	}
	return s.listingSummary(ctx, o.Filter, selected, sort, &summaryCandidates{
		visible: ids, relaxed: ids, truncated: res.Truncated || res.HasMore, selection: matchingEditions,
	})
}
