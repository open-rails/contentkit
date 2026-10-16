package hub

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestClientSearch_Integration_Keyword(t *testing.T) {
	pool := testPG(t)
	ctx := context.Background()
	schema := keywordSchema(t, ctx, pool)
	upsertDocs(t, ctx, pool, schema,
		doc("gallery", cid(1), "en", "Two factor authentication", nil, nil),
		doc("gallery", cid(2), "en", "Two factor backup codes", nil, nil),
		doc("gallery", cid(3), "en", "Cooking with cast iron", nil, nil),
	)
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	kinds := []string{"gallery"}

	// Keyword only.
	page, err := client.Search(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !reflect.DeepEqual(ids(page.Hits), []string{cid(1), cid(2)}) || page.Hits[0].TenantID != testTenant || page.Hits[0].Version() != "" || page.Hits[0].Language != "en" {
		t.Fatalf("keyword hits: %+v", page.Hits)
	}
	traced, trace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 10})
	if err != nil || !reflect.DeepEqual(traced, page) {
		t.Fatalf("traced results differ: %+v %v", traced, err)
	}
	if len(trace.Sources) != 1 || trace.Sources[0].Backend != BackendKeyword || trace.Sources[0].Status != SourceStatusSucceeded || len(trace.Results) != 2 || trace.Results[0].ScoreKind != ScoreKeywordMatch || trace.Results[0].Key.ContentID != cid(1) {
		t.Fatalf("unexpected keyword trace: %+v", trace)
	}
	limited, limitedTrace, err := client.SearchWithTrace(ctx, "factor", SearchOptions{Language: "en", ContentKinds: kinds, Limit: 1, CandidateLimit: 2})
	if err != nil || len(limited.Hits) != 1 || limitedTrace.ResultLimit != 1 || limitedTrace.CandidateLimit != 2 || len(limitedTrace.Sources[0].Candidates) != 2 || !limited.HasMore {
		t.Fatalf("candidate/result limits not separated: hits=%+v trace=%+v err=%v", limited, limitedTrace, err)
	}

	// Eligibility and typeahead.
	only := &Eligibility{SQL: "SELECT 0 AS priority WHERE sd.content_id = @allowed_id", Args: map[string]any{"allowed_id": cid(1)}}
	filtered, err := client.Search(ctx, "two factor", SearchOptions{Language: "en", ContentKinds: kinds, Eligibility: only})
	if err != nil || !reflect.DeepEqual(ids(filtered.Hits), []string{cid(1)}) {
		t.Fatalf("filtered: %+v %v", filtered, err)
	}
	suggestions, err := client.Typeahead(ctx, "two", TypeaheadOptions{Language: "en", ContentKinds: kinds, Eligibility: only})
	if err != nil || len(suggestions) != 1 || suggestions[0].ContentID != cid(1) {
		t.Fatalf("filtered typeahead: %+v %v", suggestions, err)
	}

	// Language strictness and fallback with one work in two languages.
	strict, err := client.Search(ctx, "factor", SearchOptions{Language: "es", ContentKinds: kinds})
	if err != nil || len(strict.Hits) != 0 {
		t.Fatalf("strict language: %+v %v", strict, err)
	}
	upsertDocs(t, ctx, pool, schema, doc("gallery", cid(1), "es", "Two factor authentication", nil, nil))
	fallback, err := client.Search(ctx, "two factor", SearchOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: kinds, Limit: 1})
	if err != nil || len(fallback.Hits) != 1 || fallback.Hits[0].ContentID != cid(1) || fallback.Hits[0].Language != "es" || !fallback.HasMore {
		t.Fatalf("fallback page: %+v %v", fallback, err)
	}
	next, err := client.Search(ctx, "two factor", SearchOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: kinds, Limit: 1, Offset: 1})
	if err != nil || len(next.Hits) != 1 || next.Hits[0].ContentID != cid(2) || next.Hits[0].Language != "en" || next.HasMore {
		t.Fatalf("fallback page 2: %+v %v", next, err)
	}
	fallbackTypeahead, err := client.Typeahead(ctx, "two", TypeaheadOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, ContentKinds: kinds})
	if err != nil || len(fallbackTypeahead) != 2 {
		t.Fatalf("fallback typeahead: %+v %v", fallbackTypeahead, err)
	}
	for _, h := range fallbackTypeahead {
		if h.ContentID == cid(1) && h.Language != "es" || h.ContentID == cid(2) && h.Language != "en" {
			t.Fatalf("fallback typeahead languages: %+v", fallbackTypeahead)
		}
	}
	if !strings.HasPrefix(schema, "ck_test_") {
		t.Fatal(schema)
	}
}

func TestTypeaheadOnlyMatchesNamePrefixes(t *testing.T) {
	pool := testPG(t)
	ctx := t.Context()
	schema := keywordSchema(t, ctx, pool)
	upsertDocs(t, ctx, pool, schema,
		doc("artist", grp(1), "en", "Inazuma", []string{"稲妻", "Lightning"}, nil),
		doc("gallery", cid(1), "en", "Business Question", nil, []string{"Inazuma"}),
		doc("gallery", cid(2), "en", "Inazuma Adventures", nil, nil),
		doc("gallery", cid(3), "en", "The Inazuma Story", nil, nil),
		doc("gallery", cid(4), "en", "Inazuma% Special", nil, nil),
		doc("gallery", cid(5), "en", "Inazuma_ Special", nil, nil),
		doc("gallery", cid(6), "en", "Café  Questions", []string{"Business Alias"}, nil),
	)
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"  INAZUMA  ", []string{grp(1), cid(2), cid(4), cid(5)}},
		{"稲", []string{grp(1)}},
		{"light", []string{grp(1)}},
		{"inazmua", nil}, // Full search can tolerate typos; autocomplete cannot.
		{"question", nil},
		{"inazuma%", []string{cid(4)}},
		{"inazuma_", []string{cid(5)}},
		{" CAFE   qu", []string{cid(6)}},
		{"business a", []string{cid(6)}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			hits, err := client.Typeahead(ctx, tc.query, TypeaheadOptions{ContentKinds: []string{"artist", "gallery"}})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.ContentID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("typeahead(%q) = %v; want %v", tc.query, got, tc.want)
			}
		})
	}
	full, err := client.Search(ctx, "Inazuma", SearchOptions{ContentKinds: []string{"gallery"}})
	if err != nil || !slices.Contains(ids(full.Hits), cid(1)) || !slices.Contains(ids(full.Hits), cid(3)) {
		t.Fatalf("full search lost its broader keyword matching: %+v %v", full, err)
	}
	// Apply eligibility before the limit, even when many earlier matches exist.
	only := &Eligibility{SQL: "SELECT 0 AS priority WHERE sd.content_id=@allowed", Args: map[string]any{"allowed": cid(5)}}
	hits, err := client.Typeahead(ctx, "inazuma", TypeaheadOptions{ContentKinds: []string{"gallery"}, Limit: 1, Eligibility: only})
	if err != nil || len(hits) != 1 || hits[0].ContentID != cid(5) {
		t.Fatalf("eligible prefix with limit=1: %+v %v", hits, err)
	}
}
