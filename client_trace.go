package contentkit

import "strings"

// RetrievalBackend identifies one candidate source.
type RetrievalBackend string

const (
	BackendKeyword RetrievalBackend = "keyword"
)

// ScoreKind identifies the numeric domain of a candidate or result score.
type ScoreKind string

const (
	ScoreKeywordMatch ScoreKind = "keyword_match"
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

// SourceTrace records one language-specific keyword retrieval and its candidates.
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
	RequestedResultLimit    int           `json:"requested_result_limit"`
	ResultLimit             int           `json:"result_limit"`
	RequestedCandidateLimit int           `json:"requested_candidate_limit"`
	CandidateLimit          int           `json:"candidate_limit"`
	Sources                 []SourceTrace `json:"sources,omitempty"`
	Results                 []ResultTrace `json:"results,omitempty"`
	EmptyReason             EmptyReason   `json:"empty_reason,omitempty"`
	ErrorCategory           string        `json:"error_category,omitempty"`
}

func initializeSearchTrace(client *Client, normalizedQuery string, opts SearchOptions) SearchTrace {
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = client.defaultLanguage
	}
	languages, _ := resolveLanguageModes(language, opts.LanguageMode)
	limit, _, candidateLimit := client.effectiveLimits(opts)
	return SearchTrace{
		NormalizedQuery:   normalizedQuery,
		RequestedLanguage: opts.Language, RequestedLanguageMode: opts.LanguageMode,
		Languages:            append([]string(nil), languages...),
		RequestedResultLimit: opts.Limit, ResultLimit: limit,
		RequestedCandidateLimit: opts.CandidateLimit, CandidateLimit: candidateLimit,
	}
}

func beginSourceTrace(trace *SearchTrace, backend RetrievalBackend, language string, scoreKind ScoreKind, limit int) int {
	if trace == nil {
		return -1
	}
	trace.Sources = append(trace.Sources, SourceTrace{Backend: backend, Language: language, ScoreKind: scoreKind, Limit: limit})
	return len(trace.Sources) - 1
}

// failSourceTrace records the source error that failed the request.
func failSourceTrace(trace *SearchTrace, index int, category string) {
	if trace == nil || index < 0 {
		return
	}
	trace.Sources[index].Status = SourceStatusFailed
	trace.Sources[index].ErrorCategory = category
	trace.ErrorCategory = category
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
