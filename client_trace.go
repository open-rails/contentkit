package contentkit

import "strings"

// RetrievalBackend identifies one candidate source.
type RetrievalBackend string

const (
	BackendKeyword        RetrievalBackend = "keyword"
	BackendSemanticRanker RetrievalBackend = "semantic_ranker"
)

// ScoreKind identifies the numeric domain of a candidate or result score.
type ScoreKind string

const (
	ScoreKeywordMatch   ScoreKind = "keyword_match"
	ScoreSemanticRanker ScoreKind = "semantic_ranker"
	ScoreRRF            ScoreKind = "rrf"
)

// SourceStatus records whether an attempted retrieval source succeeded.
type SourceStatus string

const (
	SourceStatusSucceeded SourceStatus = "succeeded"
	SourceStatusFailed    SourceStatus = "failed"
)

// EmptyReason explains a successful empty response.
type EmptyReason string

const (
	EmptyReasonNormalizedQuery EmptyReason = "normalized_query_empty"
	EmptyReasonNoCandidates    EmptyReason = "no_candidates"
)

// TraceKey identifies a document in retrieval provenance.
type TraceKey struct {
	ContentRef
	Language string `json:"language"`
}

// CandidateTrace records one source candidate at its raw source rank.
type CandidateTrace struct {
	Key   TraceKey `json:"key"`
	Rank  int      `json:"rank"`
	Score float32  `json:"score"`
}

// SourceTrace records one routed backend execution and its candidates. A
// failed semantic ranker source is recorded here while the request succeeds
// keyword-only (SearchResult.Degraded).
type SourceTrace struct {
	Backend       RetrievalBackend `json:"backend"`
	Language      string           `json:"language"`
	ScoreKind     ScoreKind        `json:"score_kind"`
	Limit         int              `json:"limit"`
	Status        SourceStatus     `json:"status"`
	ErrorCategory string           `json:"error_category,omitempty"`
	Candidates    []CandidateTrace `json:"candidates,omitempty"`
}

// ContributionTrace records one exact source contribution to a result score.
type ContributionTrace struct {
	SourceIndex  int     `json:"source_index"`
	SourceRank   int     `json:"source_rank"`
	Weight       float32 `json:"weight"`
	Contribution float32 `json:"contribution"`
}

// ResultTrace records one returned item and its best document's contributions.
type ResultTrace struct {
	Key           TraceKey            `json:"key"`
	Rank          int                 `json:"rank"`
	Score         float32             `json:"score"`
	ScoreKind     ScoreKind           `json:"score_kind"`
	Contributions []ContributionTrace `json:"contributions"`
}

// SearchTrace contains opt-in effective configuration and retrieval provenance.
type SearchTrace struct {
	NormalizedQuery         string        `json:"normalized_query"`
	RequestedLanguage       string        `json:"requested_language,omitempty"`
	RequestedLanguageMode   LanguageMode  `json:"requested_language_mode,omitempty"`
	Languages               []string      `json:"languages,omitempty"`
	RequestedSemantic       bool          `json:"requested_semantic"`
	Semantic                bool          `json:"semantic"`
	RequestedResultLimit    int           `json:"requested_result_limit"`
	ResultLimit             int           `json:"result_limit"`
	RequestedCandidateLimit int           `json:"requested_candidate_limit"`
	CandidateLimit          int           `json:"candidate_limit"`
	RequestedRRFK           int           `json:"requested_rrf_k"`
	RRFK                    int           `json:"rrf_k"`
	SemanticWeight          float32       `json:"semantic_weight"`
	Sources                 []SourceTrace `json:"sources,omitempty"`
	Results                 []ResultTrace `json:"results,omitempty"`
	EmptyReason             EmptyReason   `json:"empty_reason,omitempty"`
	ErrorCategory           string        `json:"error_category,omitempty"`
	// Degraded reports that the semantic ranker was requested and failed.
	Degraded bool `json:"degraded,omitempty"`
}

func initializeSearchTrace(client *Client, normalizedQuery string, opts SearchOptions) SearchTrace {
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = client.defaultLanguage
	}
	languages, _ := resolveLanguageModes(language, opts.LanguageMode)
	limit, _, candidateLimit := client.effectiveLimits(opts)
	rrfk, weight := client.fusion(opts)
	return SearchTrace{
		NormalizedQuery:   normalizedQuery,
		RequestedLanguage: opts.Language, RequestedLanguageMode: opts.LanguageMode,
		Languages:         append([]string(nil), languages...),
		RequestedSemantic: opts.Semantic, Semantic: opts.Semantic && client.ranker != nil,
		RequestedResultLimit: opts.Limit, ResultLimit: limit,
		RequestedCandidateLimit: opts.CandidateLimit, CandidateLimit: candidateLimit,
		RequestedRRFK: opts.RRFK, RRFK: rrfk, SemanticWeight: weight,
	}
}

func beginSourceTrace(trace *SearchTrace, backend RetrievalBackend, language string, scoreKind ScoreKind, limit int) int {
	if trace == nil {
		return -1
	}
	trace.Sources = append(trace.Sources, SourceTrace{Backend: backend, Language: language, ScoreKind: scoreKind, Limit: limit})
	return len(trace.Sources) - 1
}

// failSourceTrace marks a source failed; fatal says the request fails with it.
func failSourceTrace(trace *SearchTrace, index int, category string, fatal bool) {
	if trace == nil || index < 0 {
		return
	}
	trace.Sources[index].Status = SourceStatusFailed
	trace.Sources[index].ErrorCategory = category
	if fatal {
		trace.ErrorCategory = category
	} else {
		trace.Degraded = true
	}
}

func completeSourceTrace(trace *SearchTrace, index int, candidates []CandidateTrace) {
	if trace == nil || index < 0 {
		return
	}
	trace.Sources[index].Status = SourceStatusSucceeded
	trace.Sources[index].Candidates = candidates
}

// resultTraceFromGroup records a returned item: the key is the returned
// document, the score and contributions the best document's.
func resultTraceFromGroup(rank int, g group) ResultTrace {
	return ResultTrace{
		Key:   TraceKey{ContentRef: g.representative.ref, Language: g.representative.language},
		Rank:  rank,
		Score: g.best.score, ScoreKind: g.best.scoreKind,
		Contributions: g.best.contributions,
	}
}
