package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// KeywordResult is one language's scored document window.
type KeywordResult struct {
	// Hits are documents ordered by score, entity type and entity id.
	Hits []LexicalHit
	// Truncated reports that a route filled its SQL window or that more scored
	// documents existed than Limit: documents beyond the window were never ranked.
	Truncated bool
}

// KeywordSearch preserves names and aliases, including native scripts. Exact
// names precede aliases, token/prefix matches, then conservative one-edit typos.
// PostgreSQL retrieves bounded candidates; Go never scans the document catalog.
// Host filters and the eligibility join run inside every route before its limit.
func KeywordSearch(ctx context.Context, pool *pgxpool.Pool, query string, opts LexicalOptions) (KeywordResult, error) {
	var result KeywordResult
	if pool == nil || strings.TrimSpace(opts.Language) == "" {
		return result, fmt.Errorf("pool and language are required")
	}
	if opts.Limit <= 0 {
		result.Hits = []LexicalHit{}
		return result, nil
	}
	if opts.Limit > MaxCandidateLimit {
		return result, fmt.Errorf("limit must not exceed %d", MaxCandidateLimit)
	}
	if utf8.RuneCountInString(query) > 256 {
		return result, fmt.Errorf("keyword query exceeds 256 characters")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	qs, err := quoteIdent(opts.Schema)
	if err != nil {
		return result, err
	}
	// Normalize in the same database function as the expression indexes. This
	// avoids Go/Postgres Unicode casing and locale differences.
	var q, trgmSchema, nativeSchema string
	err = pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s.searchkit_keyword_normalize($1),
 (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pg_trgm'),
 (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pgroonga')`, qs), query).Scan(&q, &trgmSchema, &nativeSchema)
	if err != nil {
		return result, err
	}
	tokens := keywordTokens(q)
	if len(tokens) == 0 {
		result.Hits = []LexicalHit{}
		return result, nil
	}
	if len(tokens) > 16 {
		return result, fmt.Errorf("keyword query exceeds 16 tokens")
	}
	qt, _ := quoteIdent(trgmSchema)
	qn, _ := quoteIdent(nativeSchema)
	terms := qs + `.searchkit_keyword_terms(sd.title,sd.aliases,sd.keywords,sd.raw_document)`
	corpus := qs + `.searchkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document)`
	where := `sd.language=@language`
	sqlLimit := min(MaxCandidateLimit, max(100, opts.Limit*8))
	args := pgx.NamedArgs{"language": opts.Language, "q": q, "prefix": "", "limit": sqlLimit}
	if len(opts.EntityTypes) > 0 {
		where += ` AND sd.entity_type=ANY(@types::text[])`
		args["types"] = opts.EntityTypes
	}
	if strings.TrimSpace(opts.FilterSQL) != "" {
		where += ` AND (` + opts.FilterSQL + `)`
		if err := mergeNamedArgs(args, opts.FilterArgs); err != nil {
			return result, err
		}
	}
	from := qs + `.search_documents sd`
	parent, priority := `sd.entity_id`, `0`
	if opts.Eligibility != nil && strings.TrimSpace(opts.Eligibility.SQL) != "" {
		// LATERAL binds sd inside the host query; a missing column fails loudly.
		from += ` JOIN LATERAL (SELECT h.parent_id::text AS parent_id,h.priority::int AS priority FROM (` + opts.Eligibility.SQL + `) AS h LIMIT 1) e ON true`
		parent, priority = `e.parent_id`, `e.priority`
		if err := mergeNamedArgs(args, opts.Eligibility.Args); err != nil {
			return result, err
		}
	}
	// Tokens contain letters/numbers only and are lowercase. Appending '*' makes
	// each token a literal prefix, never an OR/NOT/query-language operator.
	prefixes := make([]string, len(tokens))
	for i, t := range tokens {
		prefixes[i] = t + "*"
	}
	args["prefix"] = strings.Join(prefixes, " ")
	selectSQL := fmt.Sprintf(`SELECT sd.entity_type,sd.entity_id,sd.language,%s,%s,%s,cardinality(sd.aliases) FROM %s WHERE %s AND `, parent, priority, terms, from, where)
	exact := selectSQL + fmt.Sprintf(`%s @> ARRAY[@q::text] ORDER BY (%s[1]=@q) DESC,(@q=ANY((%s)[2:1+cardinality(sd.aliases)])) DESC,sd.entity_type,sd.entity_id LIMIT @limit`, terms, "("+terms+")", terms)
	prefix := selectSQL + fmt.Sprintf(`%s OPERATOR(%s.&@~) @prefix ORDER BY sd.entity_type,sd.entity_id LIMIT @limit`, terms, qn)
	// The GiST word-distance order can stop after a bounded number of candidates.
	// Acceptance below requires at most one Unicode edit, not a low similarity
	// threshold. Queries of one/two characters never enter the fuzzy route.
	fuzzy := selectSQL + fmt.Sprintf(`%s OPERATOR(%s.%%>) @q ORDER BY %s OPERATOR(%s.<->>) @q,sd.entity_type,sd.entity_id LIMIT @limit`, corpus, qt, corpus, qt)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	// The whole request has a two-second ceiling (or an earlier caller deadline),
	// including candidate scoring. SET LOCAL applies before retrieval and cannot leak into
	// another pooled request. A SELECT CTE setting the GUC has ambiguous timing.
	if _, err = tx.Exec(ctx, `SET LOCAL pg_trgm.word_similarity_threshold=0.1; SET LOCAL statement_timeout=2000`); err != nil {
		return result, err
	}
	queries := []string{exact, prefix}
	if utf8.RuneCountInString(q) >= 3 {
		queries = append(queries, fuzzy)
	}
	type key struct{ kind, id, language string }
	found := map[key]LexicalHit{}
	for _, sql := range queries {
		rows, err := tx.Query(ctx, sql, args)
		if err != nil {
			return result, err
		}
		seen := 0
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return result, err
			}
			seen++
			var h LexicalHit
			var parentID *string
			var fields []string
			var aliases int
			if err := rows.Scan(&h.EntityType, &h.EntityID, &h.Language, &parentID, &h.Priority, &fields, &aliases); err != nil {
				rows.Close()
				return result, err
			}
			if parentID == nil || *parentID == "" {
				rows.Close()
				return result, fmt.Errorf("eligibility join returned no parent_id for %s/%s", h.EntityType, h.EntityID)
			}
			h.ParentID = *parentID
			h.Score = keywordScore(ctx, q, tokens, fields, aliases)
			if err := ctx.Err(); err != nil {
				rows.Close()
				return result, err
			}
			if h.Score > 0 {
				k := key{h.EntityType, h.EntityID, h.Language}
				if old, ok := found[k]; !ok || h.Score > old.Score {
					found[k] = h
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result, err
		}
		if seen >= sqlLimit {
			result.Truncated = true
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	out := make([]LexicalHit, 0, len(found))
	for _, h := range found {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].EntityType != out[j].EntityType {
			return out[i].EntityType < out[j].EntityType
		}
		return out[i].EntityID < out[j].EntityID
	})
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
		result.Truncated = true
	}
	result.Hits = out
	return result, nil
}

func keywordTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) })
}

func keywordScore(ctx context.Context, q string, tokens, fields []string, aliases int) float32 {
	if len(fields) == 0 {
		return 0
	}
	if fields[0] == q {
		return 1
	}
	for i := 1; i < len(fields) && i <= aliases; i++ {
		if fields[i] == q {
			return .9
		}
	}
	// Prefer a single name matching every token to matches scattered across
	// contextual fields, while keeping all fuzzy matches below all literal ones.
	kind := matchKeywordTokens(ctx, tokens, fields)
	if kind == 0 {
		return 0
	}
	score := float32(.75)
	if kind == 1 {
		score = .5
	}
	if matchKeywordTokens(ctx, tokens, fields[:1]) == kind {
		return score + .02
	}
	for i := 1; i < len(fields) && i <= aliases; i++ {
		if matchKeywordTokens(ctx, tokens, fields[i:i+1]) == kind {
			return score + .01
		}
	}
	return score
}

// 0: no match; 1: contains a one-edit token; 2: all tokens literal/prefix.
func matchKeywordTokens(ctx context.Context, tokens, fields []string) int {
	fuzzy := false
	for _, token := range tokens {
		if ctx.Err() != nil {
			return 0
		}
		exact, near := false, false
		for _, field := range fields {
			for _, word := range keywordTokens(field) {
				if strings.HasPrefix(word, token) || (hasNativeScript(token) && strings.Contains(word, token)) {
					exact = true
					break
				}
				if oneEdit(token, word) {
					near = true
				}
			}
			if exact {
				break
			}
		}
		if !exact && !near {
			return 0
		}
		if !exact {
			fuzzy = true
		}
	}
	if fuzzy {
		return 1
	}
	return 2
}

func hasNativeScript(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana) {
			return true
		}
	}
	return false
}

// oneEdit accepts one insertion, deletion, substitution or adjacent
// transposition in Unicode characters. Short terms require literal matching.
func oneEdit(a, b string) bool {
	x, y := []rune(a), []rune(b)
	if len(x) < 3 || len(y) < 3 || len(x) > len(y)+1 || len(y) > len(x)+1 {
		return false
	}
	i := 0
	for i < len(x) && i < len(y) && x[i] == y[i] {
		i++
	}
	if i == len(x) || i == len(y) {
		return true
	}
	if len(x) == len(y) {
		if string(x[i+1:]) == string(y[i+1:]) {
			return true
		}
		return i+1 < len(x) && x[i] == y[i+1] && x[i+1] == y[i] && string(x[i+2:]) == string(y[i+2:])
	}
	if len(x) > len(y) {
		return string(x[i+1:]) == string(y[i:])
	}
	return string(x[i:]) == string(y[i+1:])
}
