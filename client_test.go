package contentkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/search"
)

func TestNewClientRequiresTenant(t *testing.T) {
	t.Parallel()
	if _, err := NewClient(ClientConfig{Pool: newTestPool(t), Schema: "test"}); err == nil || !strings.Contains(err.Error(), "Tenant") {
		t.Fatalf("NewClient without tenant: %v", err)
	}
	c, err := NewClient(ClientConfig{Pool: newTestPool(t), Schema: "test", Tenant: " doujins "})
	if err != nil || c.Tenant() != "doujins" {
		t.Fatalf("tenant: %v %v", c, err)
	}
}

func TestClientSearchWithTrace_NormalizedEmptyMatchesSearch(t *testing.T) {
	t.Parallel()
	client, err := NewClient(ClientConfig{Pool: newTestPool(t), Schema: "test", Tenant: testTenant})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	opts := SearchOptions{ContentKinds: []string{"gallery"}}
	want, err := client.Search(context.Background(), "!!!", opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got, trace, err := client.SearchWithTrace(context.Background(), "!!!", opts)
	if err != nil {
		t.Fatalf("SearchWithTrace: %v", err)
	}
	if len(got.Hits) != 0 || len(want.Hits) != 0 || got.HasMore {
		t.Fatalf("results: got %#v, want %#v", got, want)
	}
	if trace.EmptyReason != EmptyReasonNormalizedQuery || trace.ErrorCategory != "" || len(trace.Sources) != 0 {
		t.Fatalf("unexpected trace: %#v", trace)
	}
	if trace.ResultLimit != 20 || trace.CandidateLimit != 100 {
		t.Fatalf("effective defaults missing from early trace: %#v", trace)
	}
	if _, err := json.Marshal(trace); err != nil {
		t.Fatal(err)
	}
}

func TestClientSearchWithTrace_ClampsCandidateLimit(t *testing.T) {
	t.Parallel()
	client, err := NewClient(ClientConfig{Pool: newTestPool(t), Schema: "test", Tenant: testTenant})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, trace, err := client.SearchWithTrace(context.Background(), "!!!", SearchOptions{ContentKinds: []string{"gallery"}, Limit: 10, CandidateLimit: 2})
	if err != nil {
		t.Fatalf("SearchWithTrace: %v", err)
	}
	if trace.RequestedCandidateLimit != 2 || trace.CandidateLimit != 10 {
		t.Fatalf("candidate limit was not traced/clamped: %#v", trace)
	}
}

func TestClientSearchWithTrace_Validation(t *testing.T) {
	t.Parallel()
	client, err := NewClient(ClientConfig{Pool: newTestPool(t), Schema: "test", Tenant: testTenant})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for name, opts := range map[string]SearchOptions{
		"unbounded candidate limit": {ContentKinds: []string{"gallery"}, CandidateLimit: search.MaxCandidateLimit + 1},
		"unbounded limit":           {ContentKinds: []string{"gallery"}, Limit: search.MaxCandidateLimit + 1},
		"negative offset":           {ContentKinds: []string{"gallery"}, Offset: -1},
		"no content kinds":          {},
		"invalid language mode":     {ContentKinds: []string{"gallery"}, LanguageMode: "invalid"},
	} {
		for _, query := range []string{"query", "!!!"} {
			_, trace, err := client.SearchWithTrace(context.Background(), query, opts)
			if err == nil || trace.ErrorCategory != "validation" {
				t.Fatalf("%s: SearchWithTrace(%q) error/trace = %v/%#v, want validation failure", name, query, err, trace)
			}
		}
	}
	if _, err := client.Typeahead(context.Background(), "two", TypeaheadOptions{LanguageMode: "invalid", ContentKinds: []string{"gallery"}}); err == nil {
		t.Fatal("typeahead must reject an invalid language mode")
	}
	if _, err := client.Typeahead(context.Background(), "two", TypeaheadOptions{}); err == nil {
		t.Fatal("typeahead requires content kinds")
	}
}

func TestClientSearchWithTrace_ReturnsFailedSourceTrace(t *testing.T) {
	t.Parallel()
	client, err := NewClient(ClientConfig{Pool: newTestPool(t), Schema: "test", Tenant: testTenant})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, trace, err := client.SearchWithTrace(ctx, "query", SearchOptions{Language: "en", ContentKinds: []string{"gallery"}})
	if err == nil {
		t.Fatal("SearchWithTrace() error = nil, want source error")
	}
	if trace.ErrorCategory != "keyword" || len(trace.Sources) != 1 {
		t.Fatalf("unexpected failed trace: %#v", trace)
	}
	source := trace.Sources[0]
	if source.Backend != BackendKeyword || source.ScoreKind != ScoreKeywordMatch || source.Status != SourceStatusFailed || source.ErrorCategory != "keyword" {
		t.Fatalf("unexpected failed source: %#v", source)
	}
}

func TestResolveLanguageModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		language  string
		mode      LanguageMode
		want      []string
		expectErr bool
	}{
		{"es", "", []string{"es"}, false},
		{"ja", LanguageModeExact, []string{"ja"}, false},
		{"es", LanguageModeFallbackEnglish, []string{"es", "en"}, false},
		{"en", LanguageModeFallbackEnglish, []string{"en"}, false},
		{"en", LanguageMode("invalid"), nil, true},
	} {
		got, err := resolveLanguageModes(tc.language, tc.mode)
		if tc.expectErr != (err != nil) || strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("resolveLanguageModes(%q, %q) = %v, %v", tc.language, tc.mode, got, err)
		}
	}
}
