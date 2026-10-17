package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NameSearch matches literal substrings of a document's own title or aliases.
// Exact names and prefixes precede interior matches. Contextual keywords and
// typos belong to KeywordSearch. Every indexed retrieval applies eligibility
// before its bounded limit; no catalog is scanned in Go.
func NameSearch(ctx context.Context, pool *pgxpool.Pool, query string, opts Options) (Result, error) {
	return NameSearchFrom(ctx, pool, query, opts, PublishedNames)
}

// NameSource is a trusted indexed source, never a request-supplied relation.
type NameSource uint8

const (
	PublishedNames NameSource = iota
	CurrentContentNames
	CurrentEntityNames
)

// NameSearchFrom uses the same literal matcher against current management
// names in every language. Management callers must supply their authorization
// as Eligibility; the requested display language does not hide editable names.
func NameSearchFrom(ctx context.Context, pool *pgxpool.Pool, query string, opts Options, source NameSource) (Result, error) {
	relation := "content_search_documents"
	switch source {
	case PublishedNames:
	case CurrentContentNames:
		relation = "content_management_content_names"
	case CurrentEntityNames:
		relation = "content_management_entity_names"
	default:
		return Result{}, fmt.Errorf("invalid name source")
	}
	if source != PublishedNames && (opts.Eligibility == nil || opts.Eligibility.SQL == "") {
		return Result{}, fmt.Errorf("management name search requires authorization eligibility")
	}
	return queryDocuments(ctx, pool, query, opts, func(ctx context.Context, tx pgx.Tx, query string, opts Options) (Result, error) {
		return nameSearch(ctx, tx, query, opts, relation)
	})
}

func nameSearch(ctx context.Context, tx pgx.Tx, query string, opts Options, relation string) (Result, error) {
	result := Result{Hits: []Hit{}}
	if utf8.RuneCountInString(query) > 256 {
		return result, fmt.Errorf("%w: keyword query exceeds 256 characters", ErrInvalidQuery)
	}
	sqlLimit := min(MaxCandidateLimit, max(100, opts.Limit*8))
	args := pgx.NamedArgs{"q": "", "contains": "", "limit": sqlLimit}
	from, where, priority, err := hostClausesFrom(opts, args, relation, relation != "content_search_documents")
	if err != nil {
		return result, err
	}
	qs, _ := quoteIdent(opts.Schema)
	var q, nativeSchema string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT %s.contentkit_search_normalize($1),
 (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pgroonga')`, qs), query).Scan(&q, &nativeSchema)
	if err != nil {
		return result, err
	}
	tokens := keywordTokens(q)
	if len(tokens) == 0 {
		return result, nil
	}
	if len(tokens) > 16 {
		return result, fmt.Errorf("%w: keyword query exceeds 16 tokens", ErrInvalidQuery)
	}
	args["q"] = q
	args["contains"] = literalSubstring(q)
	qn, _ := quoteIdent(nativeSchema)
	names := qs + `.contentkit_search_terms(sd.title,sd.aliases,'{}'::text[],'')`
	corpus := qs + `.contentkit_search_names(sd.title,sd.aliases)`
	// The prefix route protects strong matches from a broad substring candidate
	// cap. LIKE and strpos both treat query punctuation literally; neither route
	// accepts Groonga's query language or contextual keywords.
	base := fmt.Sprintf(`SELECT sd.content_kind,sd.content_id,sd.content_version_id,sd.language,%s,max(matched.score) AS score
 FROM %s
 CROSS JOIN LATERAL (
   SELECT max(CASE WHEN names.ordinal=1 THEN CASE WHEN name=@q THEN 1.0 WHEN starts_with(name,@q) THEN 0.9 ELSE 0.8 END
              ELSE CASE WHEN name=@q THEN 0.95 WHEN starts_with(name,@q) THEN 0.85 ELSE 0.75 END END)::real AS score
   FROM unnest(%s) WITH ORDINALITY AS names(name,ordinal)
   WHERE strpos(name,@q)>0
 ) matched
 WHERE %s AND matched.score IS NOT NULL AND `, priority, from, names, where)
	order := `
 GROUP BY 1,2,3,4,5
 ORDER BY score DESC,sd.content_kind,sd.content_id,sd.content_version_id NULLS FIRST,sd.language
 LIMIT @limit`
	queries := []string{
		base + fmt.Sprintf(`%s OPERATOR(%s.&^) @q`, names, qn) + order,
		base + corpus + ` LIKE @contains ESCAPE E'\\'` + order,
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout=2000`); err != nil {
		return result, err
	}
	found := map[documentKey]Hit{}
	for _, sql := range queries {
		rows, err := tx.Query(ctx, sql, args)
		if err != nil {
			return result, err
		}
		seen := 0
		for rows.Next() {
			seen++
			h := Hit{Language: opts.Language}
			h.TenantID = opts.Tenant
			if err := rows.Scan(&h.ContentKind, &h.ContentID, &h.ContentVersionID, &h.Language, &h.Priority, &h.Score); err != nil {
				rows.Close()
				return result, err
			}
			key := documentKey{h.Key(), h.Language}
			if old, ok := found[key]; !ok || h.Score > old.Score {
				found[key] = h
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result, err
		}
		result.Truncated = result.Truncated || seen >= sqlLimit
	}
	for _, h := range found {
		result.Hits = append(result.Hits, h)
	}
	sort.Slice(result.Hits, func(i, j int) bool { return hitLess(result.Hits[i], result.Hits[j]) })
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(result.Hits) > opts.Limit {
		result.Hits = result.Hits[:opts.Limit]
		result.Truncated = true
	}
	return result, nil
}

func literalSubstring(query string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
}
