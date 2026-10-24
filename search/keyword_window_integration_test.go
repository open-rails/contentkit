package search_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
)

// A common contextual term must not crowd stronger name matches out of the
// bounded candidate window: titles starting with the query outrank many older
// documents that only carry it as a keyword.
func TestKeywordWindowKeepsStrongestMatches(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var docs []search.KeywordDocument
	for i := range 1010 {
		d := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: contentref.New("window", "gallery", fmt.Sprintf("01920000-0000-7000-8000-%012d", i)), Language: "en"}}
		if i < 1000 {
			d.Title, d.Keywords = fmt.Sprintf("Story %d", i), []string{"romance"}
		} else {
			d.Title = fmt.Sprintf("Romance Club %d", i)
		}
		docs = append(docs, d)
	}
	if err := search.UpsertKeywordDocuments(ctx, pool, schema, docs); err != nil {
		t.Fatal(err)
	}
	res, err := search.KeywordSearch(ctx, pool, "romance", search.Options{Schema: schema, Tenant: "window", Language: "en", ContentKinds: []string{"gallery"}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range res.Hits[:10] {
		if h.Score < 0.85 {
			t.Fatalf("hit %d is %s with score %.2f; the ten title-prefix matches must lead", i, h.ContentID, h.Score)
		}
	}
}
