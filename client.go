package contentkit

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// DefaultSemanticTimeout bounds one SemanticRanker call.
const DefaultSemanticTimeout = 2 * time.Second

// ClientConfig configures the keyword search client of one tenant.
type ClientConfig struct {
	Pool   *pgxpool.Pool
	Schema string
	// Tenant scopes every document, query and result. Required.
	Tenant string

	// SemanticRanker is the optional semantic candidate source; see ports.go.
	SemanticRanker SemanticRanker
	// SemanticTimeout bounds one ranker call (default DefaultSemanticTimeout).
	SemanticTimeout time.Duration

	// Defaults.
	DefaultLanguage string
	DefaultLimit    int
	DefaultRRFK     int
}

// Client answers keyword search and typeahead for one tenant.
type Client struct {
	pool            *pgxpool.Pool
	schema          string
	tenant          string
	ranker          SemanticRanker
	semanticTimeout time.Duration

	defaultLanguage string
	defaultLimit    int
	defaultRRFK     int
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
		ranker:          cfg.SemanticRanker,
		semanticTimeout: cfg.SemanticTimeout,
		defaultLanguage: strings.TrimSpace(cfg.DefaultLanguage),
		defaultLimit:    cfg.DefaultLimit,
		defaultRRFK:     cfg.DefaultRRFK,
	}
	if c.semanticTimeout <= 0 {
		c.semanticTimeout = DefaultSemanticTimeout
	}
	if c.defaultLanguage == "" {
		c.defaultLanguage = "en"
	}
	if c.defaultLimit <= 0 {
		c.defaultLimit = 20
	}
	if c.defaultRRFK <= 0 {
		c.defaultRRFK = 60
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

	// Semantic opts into the registered SemanticRanker. Without a ranker, or
	// when the ranker fails, the request is served keyword-only.
	Semantic bool
	// SemanticWeight is the RRF weight of the semantic list against the keyword
	// list (default 1).
	SemanticWeight float32
	// RRFK is the RRF stabilizer used when fusing (default: client default).
	RRFK int

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
	// language: the keyword match tier, or the RRF score when fused.
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
	// Degraded reports that Semantic was requested and the ranker failed; the
	// page is keyword-only.
	Degraded bool
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

func (c *Client) fusion(opts SearchOptions) (rrfk int, semanticWeight float32) {
	rrfk = opts.RRFK
	if rrfk <= 0 {
		rrfk = c.defaultRRFK
	}
	semanticWeight = opts.SemanticWeight
	if semanticWeight <= 0 {
		semanticWeight = 1
	}
	return rrfk, semanticWeight
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
	semantic := opts.Semantic && c.ranker != nil
	rrfk, semanticWeight := c.fusion(opts)

	var docs []groupedDoc
	for i, lang := range languages {
		requested := i == 0
		keyword, keywordIndex, err := c.searchKeyword(ctx, userText, lang, candidateLimit, kinds, opts, trace)
		if err != nil {
			return result, err
		}
		result.Truncated = result.Truncated || keyword.Truncated
		var semanticHits []search.Hit
		semanticIndex := -1
		if semantic {
			var ok bool
			semanticHits, semanticIndex, ok = c.rankSemantic(ctx, q, lang, candidateLimit, kinds, opts, trace)
			if !ok {
				result.Degraded = true
			}
		}
		if semanticIndex < 0 {
			// Keyword ranks by calibrated match tiers; no fusion is involved.
			for rank, h := range keyword.Hits {
				docs = append(docs, groupedDoc{ref: h.ContentRef, language: h.Language, priority: h.Priority, score: h.Score, requested: requested,
					scoreKind: ScoreKeywordMatch, contributions: []ContributionTrace{{SourceIndex: keywordIndex, SourceRank: rank + 1, Weight: 1, Contribution: h.Score}}})
			}
			continue
		}
		if len(semanticHits) >= candidateLimit {
			result.Truncated = true
		}
		fused, err := fuseHits([][]search.Hit{keyword.Hits, semanticHits}, []int{keywordIndex, semanticIndex}, search.RRFOptions{K: rrfk, Weights: []float32{1, semanticWeight}}, requested)
		if err != nil {
			return fail("rrf", err)
		}
		docs = append(docs, fused...)
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

// fuseHits RRF-fuses one language's source lists into grouped documents that
// carry their exact contributions mapped onto trace source indices.
func fuseHits(lists [][]search.Hit, sourceIndexes []int, opts search.RRFOptions, requested bool) ([]groupedDoc, error) {
	keys := make([][]search.RRFKey, len(lists))
	byKey := map[search.RRFKey]search.Hit{}
	for i, list := range lists {
		keys[i] = make([]search.RRFKey, 0, len(list))
		for _, h := range list {
			k := search.RRFKey{ContentKey: h.Key(), Language: h.Language}
			keys[i] = append(keys[i], k)
			if _, ok := byKey[k]; !ok {
				byKey[k] = h
			}
		}
	}
	traced, err := search.FuseRRFWithTrace(keys, opts)
	if err != nil {
		return nil, fmt.Errorf("fusing search results: %w", err)
	}
	out := make([]groupedDoc, 0, len(traced))
	for _, t := range traced {
		h := byKey[t.Hit.RRFKey]
		contributions := make([]ContributionTrace, 0, len(t.Contributions))
		for _, c := range t.Contributions {
			contributions = append(contributions, ContributionTrace{SourceIndex: sourceIndexes[c.ListIndex], SourceRank: c.Rank, Weight: c.Weight, Contribution: c.Contribution})
		}
		out = append(out, groupedDoc{ref: h.ContentRef, language: h.Language, priority: h.Priority, score: t.Score, requested: requested, scoreKind: ScoreRRF, contributions: contributions})
	}
	return out, nil
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
		failSourceTrace(trace, traceIndex, "keyword", true)
		return result, traceIndex, err
	}
	completeSourceTrace(trace, traceIndex, candidateTraces(result.Hits))
	return result, traceIndex, nil
}

// rankSemantic asks the ranker for one language's candidates and keeps only
// those the host's eligibility join admits. A failure degrades the request to
// keyword-only: ok is false and the source trace records the category.
func (c *Client) rankSemantic(ctx context.Context, q, language string, limit int, kinds []string, opts SearchOptions, trace *SearchTrace) ([]search.Hit, int, bool) {
	traceIndex := beginSourceTrace(trace, BackendSemanticRanker, language, ScoreSemanticRanker, limit)
	rankCtx, cancel := context.WithTimeout(ctx, c.semanticTimeout)
	defer cancel()
	candidates, err := c.ranker.Rank(rankCtx, SemanticRequest{Tenant: c.tenant, Query: q, Language: language, ContentKinds: kinds, Limit: limit,
		Eligibility: opts.Eligibility, FilterSQL: opts.FilterSQL, FilterArgs: opts.FilterArgs})
	if err != nil {
		failSourceTrace(trace, traceIndex, "semantic_ranker", false)
		return nil, -1, false
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	hits, err := search.Eligible(ctx, c.pool, c.searchOptions(language, limit, kinds, opts), candidates)
	if err != nil {
		failSourceTrace(trace, traceIndex, "semantic_eligibility", false)
		return nil, -1, false
	}
	completeSourceTrace(trace, traceIndex, candidateTraces(hits))
	return hits, traceIndex, true
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
