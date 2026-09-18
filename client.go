package contentkit

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/normalize"
	"github.com/open-rails/contentkit/search"
)

type LanguageMode string

const (
	// LanguageModeExact uses only the requested language.
	LanguageModeExact LanguageMode = "exact"
	// LanguageModeFallbackEnglish uses requested language first, then English.
	LanguageModeFallbackEnglish LanguageMode = "fallback_en"
)

// ClientConfig configures the keyword search client of one tenant.
type ClientConfig struct {
	Pool   *pgxpool.Pool
	Schema string
	// Tenant scopes every document, query and result. Required.
	Tenant string

	// Defaults.
	DefaultLanguage string
	DefaultLimit    int
}

// Client answers keyword search and typeahead for one tenant.
type Client struct {
	pool   *pgxpool.Pool
	schema string
	tenant string

	defaultLanguage string
	defaultLimit    int
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Pool == nil {
		return nil, fmt.Errorf("contentkit: Pool is required")
	}
	if strings.TrimSpace(cfg.Schema) == "" {
		return nil, fmt.Errorf("contentkit: Schema is required")
	}
	if strings.TrimSpace(cfg.Tenant) == "" {
		return nil, fmt.Errorf("contentkit: Tenant is required")
	}
	c := &Client{
		pool:            cfg.Pool,
		schema:          strings.TrimSpace(cfg.Schema),
		tenant:          strings.TrimSpace(cfg.Tenant),
		defaultLanguage: strings.TrimSpace(cfg.DefaultLanguage),
		defaultLimit:    cfg.DefaultLimit,
	}
	if c.defaultLanguage == "" {
		c.defaultLanguage = "en"
	}
	if c.defaultLimit <= 0 {
		c.defaultLimit = 20
	}
	return c, nil
}

// Tenant returns the tenant this client is scoped to.
func (c *Client) Tenant() string { return c.tenant }

type SearchOptions struct {
	Language string
	// Defaults to LanguageModeExact when omitted.
	LanguageMode LanguageMode

	// ContentKinds selects the kinds searched. Required.
	ContentKinds []string

	// Limit is the page size in content items; Offset skips items. Documents
	// are grouped per item before either applies.
	Limit  int
	Offset int
	// CandidateLimit is the document window requested from each retrieval
	// source (per language) before grouping. It defaults to twice Offset+Limit
	// (at least 100) and is clamped to at least Offset+Limit. Pass the same
	// value on every page when a truncated window must stay identical.
	CandidateLimit int

	// Eligibility maps each document to the host's access, publication and
	// version-trait rules on that one document. Without it every document of
	// a work is eligible.
	Eligibility *Eligibility

	FilterSQL  string
	FilterArgs map[string]any
}

// SearchHit is one content item, represented by its matched document.
type SearchHit struct {
	// ContentRef names the matched document's content: the work, or the
	// version when the document was indexed per version.
	ContentRef
	// Language is the matched document's language.
	Language string
	// Score ranks the item by its best matching document in any searched
	// language, using the keyword match tier.
	Score float32
}

// SearchResult is one page of items.
type SearchResult struct {
	Hits []SearchHit
	// HasMore is true when items follow this page in the grouped retrieval, or
	// when Truncated: documents beyond the window were never ranked, so the
	// next page may still be non-empty.
	HasMore bool
	// Truncated reports that a candidate window filled. Raise CandidateLimit
	// for complete deep pagination.
	Truncated bool
}

// Search returns one page of content items. Documents from every searched
// language are grouped per work before Offset and Limit apply; each hit
// carries the matched document's reference and language.
func (c *Client) Search(ctx context.Context, userText string, opts SearchOptions) (SearchResult, error) {
	return c.search(ctx, userText, opts, nil)
}

// SearchWithTrace executes Search and returns opt-in retrieval provenance. On
// failure, the returned trace contains all work completed before the error.
func (c *Client) SearchWithTrace(ctx context.Context, userText string, opts SearchOptions) (SearchResult, SearchTrace, error) {
	var trace SearchTrace
	result, err := c.search(ctx, userText, opts, &trace)
	return result, trace, err
}

// effectiveLimits resolves the page and document window sizes.
func (c *Client) effectiveLimits(opts SearchOptions) (limit, offset, candidateLimit int) {
	limit = opts.Limit
	if limit <= 0 {
		limit = c.defaultLimit
	}
	offset = max(opts.Offset, 0)
	candidateLimit = opts.CandidateLimit
	if candidateLimit <= 0 {
		candidateLimit = min(search.MaxCandidateLimit, max(100, (offset+limit)*2))
	}
	candidateLimit = max(candidateLimit, offset+limit)
	return limit, offset, candidateLimit
}

func (c *Client) search(ctx context.Context, userText string, opts SearchOptions, trace *SearchTrace) (SearchResult, error) {
	var result SearchResult
	q := normalize.Query(userText)
	if trace != nil {
		*trace = initializeSearchTrace(c, q, opts)
	}
	fail := func(category string, err error) (SearchResult, error) {
		if trace != nil {
			trace.ErrorCategory = category
		}
		return result, err
	}
	if opts.Offset < 0 {
		return fail("validation", fmt.Errorf("Offset must not be negative"))
	}
	limit, offset, candidateLimit := c.effectiveLimits(opts)
	if candidateLimit > search.MaxCandidateLimit {
		return fail("validation", fmt.Errorf("effective CandidateLimit must not exceed %d", search.MaxCandidateLimit))
	}
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = c.defaultLanguage
	}
	languages, err := resolveLanguageModes(language, opts.LanguageMode)
	if err != nil {
		return fail("validation", fmt.Errorf("invalid SearchOptions.LanguageMode %q", opts.LanguageMode))
	}
	kinds := cloneAndTrim(opts.ContentKinds)
	if len(kinds) == 0 {
		return fail("validation", fmt.Errorf("ContentKinds is required"))
	}
	result.Hits = []SearchHit{}
	if q == "" || !normalize.HasAnyLetterOrNumber(q) {
		if trace != nil {
			trace.EmptyReason = EmptyReasonNormalizedQuery
		}
		return result, nil
	}

	var docs []groupedDoc
	for i, lang := range languages {
		requested := i == 0
		keyword, keywordIndex, err := c.searchKeyword(ctx, userText, lang, candidateLimit, kinds, opts, trace)
		if err != nil {
			return result, err
		}
		result.Truncated = result.Truncated || keyword.Truncated
		for rank, h := range keyword.Hits {
			docs = append(docs, groupedDoc{ref: h.ContentRef, language: h.Language, priority: h.Priority, score: h.Score, requested: requested,
				scoreKind: ScoreKeywordMatch, contributions: []ContributionTrace{{SourceIndex: keywordIndex, SourceRank: rank + 1, Weight: 1, Contribution: h.Score}}})
		}
	}
	groups := groupByContent(docs)
	if len(groups) == 0 && trace != nil {
		trace.EmptyReason = EmptyReasonNoCandidates
	}
	selected, hasMore := page(groups, offset, limit)
	result.HasMore = hasMore || result.Truncated
	result.Hits = make([]SearchHit, 0, len(selected))
	for i, g := range selected {
		result.Hits = append(result.Hits, hitFromGroup(g))
		if trace != nil {
			trace.Results = append(trace.Results, resultTraceFromGroup(offset+i+1, g))
		}
	}
	return result, nil
}

func hitFromGroup(g group) SearchHit {
	return SearchHit{ContentRef: g.representative.ref, Language: g.representative.language, Score: g.best.score}
}

func (c *Client) searchOptions(lang string, limit int, kinds []string, opts SearchOptions) search.Options {
	return search.Options{Schema: c.schema, Tenant: c.tenant, Language: lang, ContentKinds: kinds, Limit: limit,
		FilterSQL: opts.FilterSQL, FilterArgs: opts.FilterArgs, Eligibility: opts.Eligibility}
}

// searchKeyword runs one language's keyword window and returns its trace source index.
func (c *Client) searchKeyword(ctx context.Context, q string, language string, limit int, kinds []string, opts SearchOptions, trace *SearchTrace) (search.Result, int, error) {
	traceIndex := beginSourceTrace(trace, BackendKeyword, language, ScoreKeywordMatch, limit)
	result, err := search.KeywordSearch(ctx, c.pool, q, c.searchOptions(language, limit, kinds, opts))
	if err != nil {
		failSourceTrace(trace, traceIndex, "keyword")
		return result, traceIndex, err
	}
	completeSourceTrace(trace, traceIndex, candidateTraces(result.Hits))
	return result, traceIndex, nil
}

func candidateTraces(hits []search.Hit) []CandidateTrace {
	out := make([]CandidateTrace, 0, len(hits))
	for i, h := range hits {
		out = append(out, CandidateTrace{Key: TraceKey{ContentRef: h.ContentRef, Language: h.Language}, Rank: i + 1, Score: h.Score})
	}
	return out
}

type TypeaheadOptions struct {
	Language string
	// Defaults to LanguageModeExact when omitted.
	LanguageMode  LanguageMode
	ContentKinds  []string
	Limit         int
	MinSimilarity float32
	FilterSQL     string
	FilterArgs    map[string]any
	// Eligibility groups suggestions per content item; see SearchOptions.
	Eligibility *Eligibility
}

// TypeaheadHit is one suggested content item and its matched document.
type TypeaheadHit struct {
	ContentRef
	Language string
	Score    float32
}

// Typeahead returns suggestions while a user is typing (typos/substring
// matching), one per content item, grouped before Limit.
func (c *Client) Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error) {
	q := strings.TrimSpace(userText)
	if q == "" || !normalize.HasAnyLetterOrNumber(q) {
		return []TypeaheadHit{}, nil
	}
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = c.defaultLanguage
	}
	languages, err := resolveLanguageModes(language, opts.LanguageMode)
	if err != nil {
		return nil, fmt.Errorf("invalid TypeaheadOptions.LanguageMode %q", opts.LanguageMode)
	}
	kinds := cloneAndTrim(opts.ContentKinds)
	if len(kinds) == 0 {
		return nil, fmt.Errorf("ContentKinds is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > search.MaxCandidateLimit/2 {
		return nil, fmt.Errorf("Limit must not exceed %d", search.MaxCandidateLimit/2)
	}
	minSim := opts.MinSimilarity

	var docs []groupedDoc
	for i, lang := range languages {
		result, err := search.KeywordSearch(ctx, c.pool, q, search.Options{Schema: c.schema, Tenant: c.tenant, Language: lang, ContentKinds: kinds, Limit: limit * 2, FilterSQL: opts.FilterSQL, FilterArgs: opts.FilterArgs, Eligibility: opts.Eligibility})
		if err != nil {
			return nil, err
		}
		for _, h := range result.Hits {
			if minSim <= 0 || h.Score >= minSim {
				docs = append(docs, groupedDoc{ref: h.ContentRef, language: h.Language, priority: h.Priority, score: h.Score, requested: i == 0})
			}
		}
	}
	groups, _ := page(groupByContent(docs), 0, limit)
	out := make([]TypeaheadHit, 0, len(groups))
	for _, g := range groups {
		out = append(out, TypeaheadHit{ContentRef: g.representative.ref, Language: g.representative.language, Score: g.best.score})
	}
	return out, nil
}

func resolveLanguageModes(language string, mode LanguageMode) ([]string, error) {
	lang := strings.ToLower(strings.TrimSpace(language))
	if lang == "" {
		lang = "en"
	}
	switch mode {
	case "", LanguageModeExact:
		return []string{lang}, nil
	case LanguageModeFallbackEnglish:
		if lang == "en" {
			return []string{"en"}, nil
		}
		return []string{lang, "en"}, nil
	default:
		return nil, fmt.Errorf("unsupported language mode")
	}
}

func cloneAndTrim(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
