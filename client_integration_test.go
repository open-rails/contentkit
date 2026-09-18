package contentkit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// rankerFunc is a SemanticRanker test double.
type rankerFunc func(ctx context.Context, req SemanticRequest) ([]SemanticCandidate, error)

func (f rankerFunc) Rank(ctx context.Context, req SemanticRequest) ([]SemanticCandidate, error) {
	return f(ctx, req)
}

func TestClientSearch_Integration_KeywordAndSemanticRanker(t *testing.T) {
	pool := testPG(t)
	ctx := context.Background()
	schema := keywordSchema(t, ctx, pool)
	upsertDocs(t, ctx, pool, schema,
		doc("gallery", "1", "en", "Two factor authentication", nil, nil),
		doc("gallery", "2", "en", "Two factor backup codes", nil, nil),
		doc("gallery", "3", "en", "Cooking with cast iron", nil, nil),
	)
	var (
		rankerCalls []SemanticRequest
		rankerErr   error
		candidates  []SemanticCandidate
	)
	ranker := rankerFunc(func(ctx context.Context, req SemanticRequest) ([]SemanticCandidate, error) {
		rankerCalls = append(rankerCalls, req)
		if rankerErr != nil {
			return nil, rankerErr
		}
		return candidates, nil
	})
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant, SemanticRanker: ranker, SemanticTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	kinds := []string{"gallery"}

	// Keyword only.
	page, err := client.Search(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !reflect.DeepEqual(ids(page.Hits), []string{"1", "2"}) || page.Hits[0].TenantID != testTenant || page.Hits[0].Version() != "" || page.Hits[0].Language != "en" {
		t.Fatalf("keyword hits: %+v", page.Hits)
	}
	traced, trace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10})
	if err != nil || !reflect.DeepEqual(traced, page) {
		t.Fatalf("traced results differ: %+v %v", traced, err)
	}
	if len(trace.Sources) != 1 || trace.Sources[0].Backend != BackendKeyword || trace.Sources[0].Status != SourceStatusSucceeded || len(trace.Results) != 2 || trace.Results[0].ScoreKind != ScoreKeywordMatch || trace.Results[0].Key.ContentID != "1" || len(rankerCalls) != 0 {
		t.Fatalf("unexpected keyword trace: %+v", trace)
	}
	limited, limitedTrace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 1, CandidateLimit: 2})
	if err != nil || len(limited.Hits) != 1 || limitedTrace.ResultLimit != 1 || limitedTrace.CandidateLimit != 2 || len(limitedTrace.Sources[0].Candidates) != 2 || !limited.HasMore {
		t.Fatalf("candidate/result limits not separated: hits=%+v trace=%+v err=%v", limited, limitedTrace, err)
	}

	// Host filter and typeahead.
	filtered, err := client.Search(ctx, "two factor", SearchOptions{Language: "en", ContentKinds: kinds, FilterSQL: "sd.content_id = @allowed_id", FilterArgs: map[string]any{"allowed_id": "1"}})
	if err != nil || !reflect.DeepEqual(ids(filtered.Hits), []string{"1"}) {
		t.Fatalf("filtered: %+v %v", filtered, err)
	}
	suggestions, err := client.Typeahead(ctx, "two", TypeaheadOptions{Language: "en", ContentKinds: kinds, FilterSQL: "sd.content_id = @allowed_id", FilterArgs: map[string]any{"allowed_id": "1"}})
	if err != nil || len(suggestions) != 1 || suggestions[0].ContentID != "1" {
		t.Fatalf("filtered typeahead: %+v %v", suggestions, err)
	}

	// Semantic: the ranker's candidates are verified against the index and fused.
	candidates = []SemanticCandidate{
		{ContentRef: gallery("3"), Language: "en", Score: .9},
		{ContentRef: gallery("1"), Language: "en", Score: .5},
		{ContentRef: gallery("99"), Language: "en", Score: .4}, // no document: dropped
	}
	fused, fusedTrace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10, Semantic: true})
	if err != nil {
		t.Fatalf("semantic Search: %v", err)
	}
	if !reflect.DeepEqual(ids(fused.Hits), []string{"1", "3", "2"}) || fused.Degraded {
		t.Fatalf("fused hits: %+v", fused)
	}
	if len(rankerCalls) != 1 || rankerCalls[0].Tenant != testTenant || rankerCalls[0].Query != "factor" || rankerCalls[0].Language != "en" || rankerCalls[0].Limit != 100 || !reflect.DeepEqual(rankerCalls[0].ContentKinds, kinds) {
		t.Fatalf("ranker request: %+v", rankerCalls)
	}
	if len(fusedTrace.Sources) != 2 || fusedTrace.Sources[1].Backend != BackendSemanticRanker || fusedTrace.Sources[1].ScoreKind != ScoreSemanticRanker || len(fusedTrace.Sources[1].Candidates) != 2 || !fusedTrace.Semantic {
		t.Fatalf("semantic source trace: %+v", fusedTrace.Sources)
	}
	first := fusedTrace.Results[0]
	if first.ScoreKind != ScoreRRF || len(first.Contributions) != 2 || first.Contributions[0].SourceIndex != 0 || first.Contributions[1].SourceIndex != 1 {
		t.Fatalf("fused result must carry both sources' contributions: %+v", first)
	}
	var sum float32
	for _, c := range first.Contributions {
		sum += c.Contribution
	}
	if sum != first.Score {
		t.Fatalf("contributions %v != score %v", sum, first.Score)
	}
	plainFused, err := client.Search(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10, Semantic: true})
	if err != nil || !reflect.DeepEqual(plainFused, fused) {
		t.Fatalf("semantic Search must match SearchWithTrace: plain=%+v traced=%+v err=%v", plainFused, fused, err)
	}
	// Eligibility applies to semantic candidates as to keyword documents.
	eligible, err := client.Search(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Semantic: true,
		Eligibility: &Eligibility{SQL: `SELECT 0 AS priority WHERE sd.content_id = @only`, Args: map[string]any{"only": "2"}}})
	if err != nil || !reflect.DeepEqual(ids(eligible.Hits), []string{"2"}) {
		t.Fatalf("eligibility must bound semantic candidates: %+v %v", eligible, err)
	}
	// A ranker proposing another tenant's content degrades the request.
	candidates = []SemanticCandidate{{ContentRef: contentref.New("hentai0", "gallery", "3"), Language: "en", Score: .9}}
	foreign, foreignTrace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Semantic: true})
	if err != nil || !foreign.Degraded || !reflect.DeepEqual(ids(foreign.Hits), []string{"1", "2"}) || foreignTrace.Sources[1].ErrorCategory != "semantic_eligibility" {
		t.Fatalf("foreign candidate: %+v %+v %v", foreign, foreignTrace.Sources, err)
	}

	// Ranker failure degrades to keyword-only, never an error.
	rankerErr = errors.New("provider down")
	degraded, degradedTrace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10, Semantic: true})
	if err != nil || !degraded.Degraded || !reflect.DeepEqual(ids(degraded.Hits), []string{"1", "2"}) {
		t.Fatalf("degraded: %+v %v", degraded, err)
	}
	if !degradedTrace.Degraded || degradedTrace.ErrorCategory != "" || degradedTrace.Sources[1].Status != SourceStatusFailed || degradedTrace.Sources[1].ErrorCategory != "semantic_ranker" || degradedTrace.Results[0].ScoreKind != ScoreKeywordMatch {
		t.Fatalf("degraded trace: %+v", degradedTrace)
	}
	// A hung ranker is bounded by SemanticTimeout.
	rankerErr = nil
	slow := rankerFunc(func(ctx context.Context, _ SemanticRequest) ([]SemanticCandidate, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	slowClient, _ := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant, SemanticRanker: slow, SemanticTimeout: 50 * time.Millisecond})
	started := time.Now()
	hung, err := slowClient.Search(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Semantic: true})
	if err != nil || !hung.Degraded || len(hung.Hits) != 2 || time.Since(started) > 2*time.Second {
		t.Fatalf("hung ranker: %+v %v after %s", hung, err, time.Since(started))
	}
	// Without a registered ranker, Semantic is keyword-only and not degraded.
	plain, _ := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant})
	none, noneTrace, err := plain.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Semantic: true})
	if err != nil || none.Degraded || len(noneTrace.Sources) != 1 || noneTrace.Semantic || !noneTrace.RequestedSemantic {
		t.Fatalf("no ranker: %+v %+v %v", none, noneTrace, err)
	}

	// Language strictness and fallback with one work in two languages.
	strict, err := client.Search(ctx, "factor", SearchOptions{Language: "es", ContentKinds: kinds})
	if err != nil || len(strict.Hits) != 0 {
		t.Fatalf("strict language: %+v %v", strict, err)
	}
	upsertDocs(t, ctx, pool, schema, doc("gallery", "1", "es", "Two factor authentication", nil, nil))
	fallback, err := client.Search(ctx, "two factor", SearchOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: kinds, Limit: 1})
	if err != nil || len(fallback.Hits) != 1 || fallback.Hits[0].ContentID != "1" || fallback.Hits[0].Language != "es" || !fallback.HasMore {
		t.Fatalf("fallback page: %+v %v", fallback, err)
	}
	next, err := client.Search(ctx, "two factor", SearchOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: kinds, Limit: 1, Offset: 1})
	if err != nil || len(next.Hits) != 1 || next.Hits[0].ContentID != "2" || next.Hits[0].Language != "en" || next.HasMore {
		t.Fatalf("fallback page 2: %+v %v", next, err)
	}
	fallbackTypeahead, err := client.Typeahead(ctx, "two", TypeaheadOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: kinds})
	if err != nil || len(fallbackTypeahead) != 2 {
		t.Fatalf("fallback typeahead: %+v %v", fallbackTypeahead, err)
	}
	for _, h := range fallbackTypeahead {
		if h.ContentID == "1" && h.Language != "es" || h.ContentID == "2" && h.Language != "en" {
			t.Fatalf("fallback typeahead languages: %+v", fallbackTypeahead)
		}
	}
	if !strings.HasPrefix(schema, "ck_test_") {
		t.Fatal(schema)
	}
}
