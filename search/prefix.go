package search

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PrefixSearch matches only the beginning of a document's title or aliases.
// Contextual keywords, interior words and typos belong to KeywordSearch, not
// autocomplete. It shares search normalization and eligibility, with a dedicated
// name-prefix index.
func PrefixSearch(ctx context.Context, pool *pgxpool.Pool, query string, opts Options) (Result, error) {
	return queryDocuments(ctx, pool, query, opts, prefixSearch)
}

func prefixSearch(ctx context.Context, tx pgx.Tx, query string, opts Options) (Result, error) {
	result := Result{Hits: []Hit{}}
	if utf8.RuneCountInString(query) > 256 {
		return result, fmt.Errorf("keyword query exceeds 256 characters")
	}
	sqlLimit := min(MaxCandidateLimit, opts.Limit+1)
	args := pgx.NamedArgs{"q": "", "limit": sqlLimit}
	from, where, priority, err := hostClauses(opts, args)
	if err != nil {
		return result, err
	}
	qs, _ := quoteIdent(opts.Schema)
	var q, nativeSchema string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT %s.contentkit_keyword_normalize($1),
 (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pgroonga')`, qs), query).Scan(&q, &nativeSchema)
	if err != nil {
		return result, err
	}
	tokens := keywordTokens(q)
	if len(tokens) == 0 {
		return result, nil
	}
	if len(tokens) > 16 {
		return result, fmt.Errorf("keyword query exceeds 16 tokens")
	}
	args["q"] = q
	qn, _ := quoteIdent(nativeSchema)
	// A term-prefix index covers only names, not contextual keywords. Unlike
	// full-text token prefixes, this also handles complete CJK names correctly.
	// starts_with verifies literal normalized spelling, independent of the
	// extension's own normalizer. Neither predicate interprets query syntax.
	names := qs + `.contentkit_keyword_terms(sd.title,sd.aliases,'{}'::text[],'')`
	sql := fmt.Sprintf(`SELECT sd.content_kind,sd.content_id,sd.content_version_id,sd.language,%s,matched.score
 FROM %s
 CROSS JOIN LATERAL (
   SELECT max(CASE WHEN names.ordinal=1 THEN CASE WHEN name=@q THEN 1.0 ELSE 0.9 END
              ELSE CASE WHEN name=@q THEN 0.95 ELSE 0.85 END END)::real AS score
   FROM unnest(%s) WITH ORDINALITY AS names(name,ordinal)
   WHERE starts_with(name,@q)
 ) matched
 WHERE %s AND %s OPERATOR(%s.&^) @q AND matched.score IS NOT NULL
 ORDER BY matched.score DESC,sd.content_kind,sd.content_id,sd.content_version_id NULLS FIRST
 LIMIT @limit`, priority, from, names, where, names, qn)
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout=2000`); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, sql, args)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		h := Hit{Language: opts.Language}
		h.TenantID = opts.Tenant
		if err := rows.Scan(&h.ContentKind, &h.ContentID, &h.ContentVersionID, &h.Language, &h.Priority, &h.Score); err != nil {
			return result, err
		}
		result.Hits = append(result.Hits, h)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.Truncated = len(result.Hits) == MaxCandidateLimit
	if len(result.Hits) > opts.Limit {
		result.Hits = result.Hits[:opts.Limit]
		result.Truncated = true
	}
	return result, nil
}
