package searchkit

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	querynorm "github.com/open-rails/searchkit/internal/normalize"
	"github.com/open-rails/searchkit/search"
)

type Embedder interface {
	EmbedQueryText(ctx context.Context, model string, text string) ([]float32, error)
}

type SearchMode string

const (
	SearchModeLexical  SearchMode = "lexical"
	SearchModeSemantic SearchMode = "semantic"
	SearchModeDual     SearchMode = "dual"
)

type LanguageMode string

const (
	// LanguageModeExact uses only the requested language.
	LanguageModeExact LanguageMode = "exact"
	// LanguageModeFallbackEnglish uses requested language first, then English.
	LanguageModeFallbackEnglish LanguageMode = "fallback_en"
)

type ClientConfig struct {
	Pool   *pgxpool.Pool
	Schema string

	Embedder Embedder

	// Defaults.
	DefaultLanguage  string
	DefaultModel     string
	DefaultLimit     int
	DefaultRRFK      int
	TwoStage         bool
	OversampleFactor int
}

type Client struct {
	pool     *pgxpool.Pool
	schema   string
	embedder Embedder

	defaultLanguage   string
	defaultModel      string
	defaultLimit      int
	defaultRRFK       int
	defaultTwoStage   bool
	defaultOversample int
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Pool == nil {
		return nil, fmt.Errorf("Pool is required")
	}
	if strings.TrimSpace(cfg.Schema) == "" {
		return nil, fmt.Errorf("Schema is required")
	}
	c := &Client{
		pool:              cfg.Pool,
		schema:            strings.TrimSpace(cfg.Schema),
		embedder:          cfg.Embedder,
		defaultLanguage:   strings.TrimSpace(cfg.DefaultLanguage),
		defaultModel:      strings.TrimSpace(cfg.DefaultModel),
		defaultLimit:      cfg.DefaultLimit,
		defaultRRFK:       cfg.DefaultRRFK,
		defaultTwoStage:   cfg.TwoStage,
		defaultOversample: cfg.OversampleFactor,
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
	if c.defaultOversample < 0 {
		c.defaultOversample = 0
	}
	return c, nil
}

type SearchOptions struct {
	Language string
	// Defaults to LanguageModeExact when omitted.
	LanguageMode LanguageMode
	// Defaults to lexical; semantic and dual require explicit opt-in.
	Mode SearchMode

	// If set, applied to both lexical + semantic entity types unless explicitly overridden.
	EntityTypes []string

	LexicalEntityTypes  []string
	SemanticEntityTypes []string

	// Limit is the page size in content items; Offset skips items. Documents
	// are grouped per item before either applies.
	Limit  int
	Offset int
	// CandidateLimit is the document window requested from each retrieval
	// source (per language) before grouping. It defaults to twice Offset+Limit
	// (at least 100) and is clamped to at least Offset+Limit. Pass the same
	// value on every page when a truncated window must stay identical.
	CandidateLimit int

	// Eligibility maps each document to its content item and enforces the
	// host's access, publication and version-trait rules on that one document.
	// Lexical mode only. Without it every document is its own item.
	Eligibility *Eligibility

	// Semantic model override (defaults to client).
	Model string

	TwoStage         *bool
	OversampleFactor int
	RRFK             int
	// SemanticMinSimilarity drops semantic candidates below this cosine
	// similarity before RRF. Values <= 0 disable the additional floor.
	SemanticMinSimilarity float32
	// SemanticMinSimilarityEnabled applies SemanticMinSimilarity even when it is
	// zero. This preserves an inclusive non-negative floor while the zero value
	// of both fields continues to disable filtering.
	SemanticMinSimilarityEnabled bool

	FilterSQL  string
	FilterArgs map[string]any
}

// SearchHit is one content item, represented by its matched document.
type SearchHit struct {
	EntityType string
	// EntityID is the matched document (for example a version id) and
	// ParentID the item it belongs to; they are equal without Eligibility.
	EntityID string
	ParentID string
	// Language is the matched document's language.
	Language string
	// Score ranks the item by its best matching document in any searched
	// language: the keyword match tier in lexical mode, RRF otherwise.
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

type SimilarOptions struct {
	Language string
	Model    string
	Limit    int

	EntityTypes []string
	ExcludeIDs  []string

	MinSimilarity        float32
	MinSimilarityEnabled bool

	FilterSQL  string
	FilterArgs map[string]any
}

type SimilarHit struct {
	EntityType string
	EntityID   string
	Model      string
	Language   string
	Score      float32
}

// Search returns one page of content items. Documents from every searched
// language are grouped per item (Eligibility parent, else entity id) before
// Offset and Limit apply; each hit carries the matched document and language.
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
	qEmbed := querynorm.QueryForEmbedding(userText)
	if trace != nil {
		*trace = initializeSearchTrace(c, qEmbed, opts)
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
	if math.IsNaN(float64(opts.SemanticMinSimilarity)) || math.IsInf(float64(opts.SemanticMinSimilarity), 0) {
		return fail("validation", fmt.Errorf("SemanticMinSimilarity must be finite"))
	}
	mode := opts.Mode
	if mode == "" {
		mode = SearchModeLexical
	}
	switch mode {
	case SearchModeLexical, SearchModeSemantic, SearchModeDual:
	default:
		return fail("validation", fmt.Errorf("invalid SearchOptions.Mode %q", mode))
	}
	if opts.Eligibility != nil && mode != SearchModeLexical {
		return fail("validation", fmt.Errorf("Eligibility requires SearchModeLexical"))
	}
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = c.defaultLanguage
	}
	languages, err := resolveLanguageModes(language, opts.LanguageMode)
	if err != nil {
		return fail("validation", fmt.Errorf("invalid SearchOptions.LanguageMode %q", opts.LanguageMode))
	}
	result.Hits = []SearchHit{}
	if qEmbed == "" || !hasAnyLetterOrNumber(qEmbed) {
		if trace != nil {
			trace.EmptyReason = EmptyReasonNormalizedQuery
		}
		return result, nil
	}

	semanticMinSimilarity := opts.SemanticMinSimilarity
	semanticMinSimilarityEnabled := opts.SemanticMinSimilarityEnabled || semanticMinSimilarity > 0
	if semanticMinSimilarity <= 0 && !semanticMinSimilarityEnabled {
		semanticMinSimilarity = 0
	}

	rrfk := opts.RRFK
	if rrfk <= 0 {
		rrfk = c.defaultRRFK
	}
	if trace != nil {
		trace.Mode = mode
		trace.Languages = append([]string(nil), languages...)
		trace.ResultLimit = limit
		trace.CandidateLimit = candidateLimit
		trace.RRFK = rrfk
		trace.SemanticMinSimilarity = semanticMinSimilarity
		trace.SemanticMinSimilarityEnabled = semanticMinSimilarityEnabled
	}

	lexTypes := cloneAndTrim(opts.LexicalEntityTypes)
	semTypes := cloneAndTrim(opts.SemanticEntityTypes)
	if len(opts.EntityTypes) > 0 {
		all := cloneAndTrim(opts.EntityTypes)
		if len(lexTypes) == 0 {
			lexTypes = all
		}
		if len(semTypes) == 0 {
			semTypes = all
		}
	}

	if mode != SearchModeSemantic && len(lexTypes) == 0 {
		return fail("validation", fmt.Errorf("LexicalEntityTypes is required for lexical/dual search"))
	}
	if mode != SearchModeLexical && len(semTypes) == 0 {
		return fail("validation", fmt.Errorf("SemanticEntityTypes is required for semantic/dual search"))
	}

	// Lexical mode ranks by calibrated keyword tiers and groups per item across
	// the searched languages; no rank fusion is involved.
	if mode == SearchModeLexical {
		var docs []groupedDoc
		for _, lang := range languages {
			keyword, sourceIndex, err := c.searchLexical(ctx, userText, lang, candidateLimit, lexTypes, opts.FilterSQL, opts.FilterArgs, opts.Eligibility, trace)
			if err != nil {
				return result, err
			}
			result.Truncated = result.Truncated || keyword.Truncated
			for rank, h := range keyword.Hits {
				docs = append(docs, groupedDoc{
					EntityType: h.EntityType, EntityID: h.EntityID, ParentID: h.ParentID, Language: h.Language,
					Priority: h.Priority, Score: h.Score, requested: lang == languages[0], sourceIndex: sourceIndex, sourceRank: rank + 1,
				})
			}
		}
		groups := groupByParent(docs)
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

	lists := make([][]search.RRFKey, 0, 4)
	if mode == SearchModeDual {
		for _, lang := range languages {
			keyword, _, err := c.searchLexical(ctx, userText, lang, candidateLimit, lexTypes, opts.FilterSQL, opts.FilterArgs, nil, trace)
			if err != nil {
				return result, err
			}
			result.Truncated = result.Truncated || keyword.Truncated
			keys := make([]search.RRFKey, 0, len(keyword.Hits))
			for _, h := range keyword.Hits {
				keys = append(keys, search.RRFKey{EntityType: h.EntityType, EntityID: h.EntityID, Language: h.Language})
			}
			lists = append(lists, keys)
		}
	}

	if c.embedder == nil {
		return fail("embedder_required", fmt.Errorf("Embedder is required for semantic search"))
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = c.defaultModel
	}
	if strings.TrimSpace(model) == "" {
		return fail("model_required", fmt.Errorf("Model is required for semantic search"))
	}

	twoStage := c.defaultTwoStage
	if opts.TwoStage != nil {
		twoStage = *opts.TwoStage
	}
	oversample := opts.OversampleFactor
	if oversample <= 0 {
		oversample = c.defaultOversample
	}
	oversample = search.EffectiveOversampleFactor(oversample)
	if trace != nil {
		trace.Model = model
		trace.TwoStage = twoStage
		trace.OversampleFactor = oversample
	}

	vec, err := c.embedder.EmbedQueryText(ctx, model, qEmbed)
	if err != nil {
		return fail("embedding", err)
	}
	if len(vec) == 0 {
		if trace != nil {
			trace.EmptyReason = EmptyReasonEmbedding
		}
		return result, nil
	}

	for _, lang := range languages {
		semKeys, err := c.searchSemantic(ctx, lang, model, vec, candidateLimit, semTypes, twoStage, oversample, semanticMinSimilarity, semanticMinSimilarityEnabled, opts.FilterSQL, opts.FilterArgs, trace)
		if err != nil {
			return result, err
		}
		if len(semKeys) >= candidateLimit {
			result.Truncated = true
		}
		lists = append(lists, semKeys)
	}

	var fused []search.RRFHit
	var traced []search.RRFTraceHit
	if trace == nil {
		fused = search.FuseRRF(lists, search.RRFOptions{K: rrfk})
	} else {
		traced, err = search.FuseRRFWithTrace(lists, search.RRFOptions{K: rrfk})
		if err != nil {
			return fail("rrf", fmt.Errorf("fusing traced search results: %w", err))
		}
		fused = make([]search.RRFHit, 0, len(traced))
		for _, hit := range traced {
			fused = append(fused, hit.Hit)
		}
	}
	if len(fused) == 0 && trace != nil {
		trace.EmptyReason = EmptyReasonNoCandidates
	}
	// Fused keys still carry one entry per language; group them per entity so a
	// fallback language never repeats an item.
	docs := make([]groupedDoc, 0, len(fused))
	for i, h := range fused {
		docs = append(docs, groupedDoc{
			EntityType: h.EntityType, EntityID: h.EntityID, ParentID: h.EntityID, Language: h.Language,
			Score: h.Score, requested: h.Language == languages[0], fused: i,
		})
	}
	groups := groupByParent(docs)
	selected, hasMore := page(groups, offset, limit)
	result.HasMore = hasMore || result.Truncated
	result.Hits = make([]SearchHit, 0, len(selected))
	for i, g := range selected {
		result.Hits = append(result.Hits, hitFromGroup(g))
		if trace != nil {
			resultTrace := resultTraceFromRRF(offset+i+1, traced[g.best.fused])
			resultTrace.Key = TraceKey{EntityType: g.representative.EntityType, EntityID: g.representative.EntityID, ParentID: g.representative.ParentID, Language: g.representative.Language}
			trace.Results = append(trace.Results, resultTrace)
		}
	}
	return result, nil
}

func hitFromGroup(g group) SearchHit {
	return SearchHit{
		EntityType: g.representative.EntityType,
		EntityID:   g.representative.EntityID,
		ParentID:   g.representative.ParentID,
		Language:   g.representative.Language,
		Score:      g.best.Score,
	}
}

func (c *Client) SimilarTo(ctx context.Context, entityType string, entityID string, opts SimilarOptions) ([]SimilarHit, error) {
	lang := strings.TrimSpace(opts.Language)
	if lang == "" {
		lang = c.defaultLanguage
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = c.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("Model is required for similarity search")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = c.defaultLimit
	}
	if strings.TrimSpace(entityType) == "" || strings.TrimSpace(entityID) == "" {
		return nil, fmt.Errorf("entityType and entityID are required")
	}

	rows, err := search.SimilarTo(ctx, c.pool, c.schema, entityType, entityID, model, lang, limit, search.Options{
		EntityTypes:          cloneAndTrim(opts.EntityTypes),
		ExcludeIDs:           cloneAndTrim(opts.ExcludeIDs),
		MinSimilarity:        opts.MinSimilarity,
		MinSimilarityEnabled: opts.MinSimilarityEnabled,
		FilterSQL:            opts.FilterSQL,
		FilterArgs:           opts.FilterArgs,
	})
	if err != nil {
		return nil, err
	}

	out := make([]SimilarHit, 0, len(rows))
	for _, row := range rows {
		out = append(out, SimilarHit{
			EntityType: row.EntityType,
			EntityID:   row.EntityID,
			Model:      row.Model,
			Language:   row.Language,
			Score:      row.Similarity,
		})
	}
	return out, nil
}

// searchLexical runs one language's keyword window and returns its trace source index.
func (c *Client) searchLexical(ctx context.Context, q string, language string, limit int, entityTypes []string, filterSQL string, filterArgs map[string]any, eligibility *Eligibility, trace *SearchTrace) (search.KeywordResult, int, error) {
	traceIndex := beginSourceTrace(trace, BackendKeyword, language, "", ScoreKeywordMatch, limit)
	result, err := search.KeywordSearch(ctx, c.pool, q, search.LexicalOptions{Schema: c.schema, Language: language, EntityTypes: entityTypes, Limit: limit, FilterSQL: filterSQL, FilterArgs: filterArgs, Eligibility: eligibility})
	if err != nil {
		failSourceTrace(trace, traceIndex, "keyword")
		return result, traceIndex, err
	}
	if trace != nil {
		candidates := make([]CandidateTrace, 0, len(result.Hits))
		for i, h := range result.Hits {
			candidates = append(candidates, CandidateTrace{Key: TraceKey{EntityType: h.EntityType, EntityID: h.EntityID, ParentID: h.ParentID, Language: h.Language}, Rank: i + 1, Score: h.Score})
		}
		completeSourceTrace(trace, traceIndex, candidates)
	}
	return result, traceIndex, nil
}

func (c *Client) searchSemantic(
	ctx context.Context,
	language string,
	model string,
	queryVec []float32,
	limit int,
	entityTypes []string,
	twoStage bool,
	oversampleFactor int,
	minSimilarity float32,
	minSimilarityEnabled bool,
	filterSQL string,
	filterArgs map[string]any,
	trace *SearchTrace,
) ([]search.RRFKey, error) {
	traceIndex := beginSourceTrace(trace, BackendSemantic, language, model, ScoreCosineSimilarity, limit)
	sem, err := search.SemanticSearch(ctx, c.pool, search.Query{
		Schema:     c.schema,
		Model:      model,
		Language:   language,
		QueryVec:   queryVec,
		Limit:      limit,
		Dimensions: len(queryVec),
		Options: search.Options{
			EntityTypes:          entityTypes,
			MinSimilarity:        minSimilarity,
			MinSimilarityEnabled: minSimilarityEnabled,
			TwoStage:             twoStage,
			OversampleFactor:     oversampleFactor,
			FilterSQL:            filterSQL,
			FilterArgs:           filterArgs,
		},
	})
	if err != nil {
		failSourceTrace(trace, traceIndex, "semantic")
		return nil, err
	}
	keys := make([]search.RRFKey, 0, len(sem))
	var candidates []CandidateTrace
	if trace != nil {
		candidates = make([]CandidateTrace, 0, len(sem))
	}
	for i, h := range sem {
		keys = append(keys, search.RRFKey{EntityType: h.EntityType, EntityID: h.EntityID, Language: h.Language})
		if trace != nil {
			candidates = append(candidates, CandidateTrace{
				Key:  TraceKey{EntityType: h.EntityType, EntityID: h.EntityID, Language: h.Language},
				Rank: i + 1, Score: h.Similarity,
			})
		}
	}
	completeSourceTrace(trace, traceIndex, candidates)
	return keys, nil
}

type TypeaheadOptions struct {
	Language string
	// Defaults to LanguageModeExact when omitted.
	LanguageMode  LanguageMode
	EntityTypes   []string
	Limit         int
	MinSimilarity float32
	FilterSQL     string
	FilterArgs    map[string]any
	// Eligibility groups suggestions per content item; see SearchOptions.
	Eligibility *Eligibility
}

// TypeaheadHit is one suggested content item and its matched document.
type TypeaheadHit struct {
	EntityType string
	EntityID   string
	ParentID   string
	Language   string
	Score      float32
}

// Typeahead returns suggestions while a user is typing (typos/substring
// matching), one per content item, grouped before Limit.
func (c *Client) Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error) {
	q := strings.TrimSpace(userText)
	if q == "" || !hasAnyLetterOrNumber(q) {
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
	entityTypes := cloneAndTrim(opts.EntityTypes)
	if len(entityTypes) == 0 {
		return nil, fmt.Errorf("EntityTypes is required")
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
	for _, lang := range languages {
		result, err := search.KeywordSearch(ctx, c.pool, q, search.LexicalOptions{Schema: c.schema, Language: lang, EntityTypes: entityTypes, Limit: limit * 2, FilterSQL: opts.FilterSQL, FilterArgs: opts.FilterArgs, Eligibility: opts.Eligibility})
		if err != nil {
			return nil, err
		}
		for _, h := range result.Hits {
			if minSim <= 0 || h.Score >= minSim {
				docs = append(docs, groupedDoc{EntityType: h.EntityType, EntityID: h.EntityID, ParentID: h.ParentID, Language: h.Language, Priority: h.Priority, Score: h.Score, requested: lang == languages[0]})
			}
		}
	}
	groups, _ := page(groupByParent(docs), 0, limit)
	out := make([]TypeaheadHit, 0, len(groups))
	for _, g := range groups {
		out = append(out, TypeaheadHit{EntityType: g.representative.EntityType, EntityID: g.representative.EntityID, ParentID: g.representative.ParentID, Language: g.representative.Language, Score: g.best.Score})
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
