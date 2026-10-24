package search

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
)

type documentKey struct {
	contentref.ContentKey
	language string
}

// KeywordSearch preserves names and aliases, including native scripts. Exact
// names precede aliases, name prefixes/substrings, contextual token matches,
// then conservative one-edit typos. Common English fillers are search-only
// normalization, never entity identity normalization.
// PostgreSQL ranks literal candidates by match tier over the stored terms, so
// a bounded window keeps the strongest matches; Go then scores that window.
// The eligibility join runs inside every route before its limit.
func KeywordSearch(ctx context.Context, pool *pgxpool.Pool, query string, opts Options) (Result, error) {
	return queryDocuments(ctx, pool, query, opts, keywordSearch)
}

func queryDocuments(ctx context.Context, pool *pgxpool.Pool, query string, opts Options, retrieve func(context.Context, pgx.Tx, string, Options) (Result, error)) (Result, error) {
	if pool == nil {
		return Result{}, fmt.Errorf("pool is required")
	}
	if opts.Limit <= 0 {
		return Result{Hits: []Hit{}}, nil
	}
	if opts.Limit > MaxCandidateLimit {
		return Result{}, fmt.Errorf("limit must not exceed %d", MaxCandidateLimit)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	result, err := retrieve(ctx, tx, query, opts)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

// keywordQuery is one normalized request and the SQL operators it uses.
type keywordQuery struct {
	q        string
	tokens   []string
	prefix   string
	contains string
	// trgm and native are the schemas of pg_trgm and PGroonga, quoted.
	trgm, native string
}

func prepareKeyword(ctx context.Context, tx pgx.Tx, schema, query string) (keywordQuery, error) {
	var k keywordQuery
	if utf8.RuneCountInString(query) > 256 {
		return k, fmt.Errorf("%w: keyword query exceeds 256 characters", ErrInvalidQuery)
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return k, err
	}
	// Normalize in the same database function as the stored terms. This avoids
	// Go/Postgres Unicode casing and locale differences.
	var trgm, native string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT %s.contentkit_search_normalize($1),
 (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pg_trgm'),
 (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pgroonga')`, qs), query).Scan(&k.q, &trgm, &native)
	if err != nil {
		return k, err
	}
	k.tokens = keywordTokens(k.q)
	if len(k.tokens) > 16 {
		return k, fmt.Errorf("%w: keyword query exceeds 16 tokens", ErrInvalidQuery)
	}
	k.trgm, _ = quoteIdent(trgm)
	k.native, _ = quoteIdent(native)
	k.contains = literalSubstring(k.q)
	// Tokens contain letters/numbers only and are lowercase. Appending '*' makes
	// each token a literal prefix, never an OR/NOT/query-language operator.
	prefixes := make([]string, len(k.tokens))
	for i, t := range k.tokens {
		prefixes[i] = t + "*"
	}
	k.prefix = strings.Join(prefixes, " ")
	return k, nil
}

// fuzzy reports whether typo tolerance applies. Its trigram prefilter admits
// nearly every row, so it is always a bounded nearest-distance window; Go
// accepts at most one edit.
func (k keywordQuery) fuzzy() bool { return utf8.RuneCountInString(k.q) >= 3 }

// literalRoutes match what keywordScore accepts without a typo: the exact term,
// every token as a word prefix, or the query inside a name (or, for a
// native-script query, inside any term). Each route selects cols from the
// documents filtered by where through its own index; combined in one WHERE,
// the planner would scan and evaluate PGroonga row by row.
func (k keywordQuery) literalRoutes(cols, from, where, q, prefix, contains string) []string {
	contained := ` AND EXISTS (SELECT 1 FROM unnest(sd.terms[1:1+cardinality(sd.aliases)]) AS name WHERE strpos(name,` + q + `)>0)`
	if hasNativeScript(k.q) {
		contained = ``
	}
	base := `SELECT ` + cols + ` FROM ` + from + ` WHERE ` + where + ` AND `
	return []string{
		base + `sd.terms @> ARRAY[` + q + `::text]`,
		base + fmt.Sprintf(`sd.terms OPERATOR(%s.&@~) %s`, k.native, prefix),
		base + `sd.body LIKE ` + contains + ` ESCAPE E'\\'` + contained,
	}
}

// tierSQL orders literal candidates as keywordScore ranks them by name, over
// rows carrying terms and their alias count; contextual token matches share
// the lowest literal tier.
const tierSQL = `CASE WHEN sd.terms[1]=@q THEN 4
 WHEN @q=ANY(sd.terms[2:1+sd.aliases]) THEN 3
 WHEN starts_with(sd.terms[1],@q) THEN 2
 WHEN EXISTS (SELECT 1 FROM unnest(sd.terms[2:1+sd.aliases]) AS name WHERE starts_with(name,@q)) THEN 2
 WHEN strpos(sd.terms[1],@q)>0 THEN 1
 WHEN EXISTS (SELECT 1 FROM unnest(sd.terms[2:1+sd.aliases]) AS name WHERE strpos(name,@q)>0) THEN 1
 ELSE 0 END`

func keywordSearch(ctx context.Context, tx pgx.Tx, query string, opts Options) (Result, error) {
	var result Result
	k, err := prepareKeyword(ctx, tx, opts.Schema, query)
	if err != nil {
		return result, err
	}
	if len(k.tokens) == 0 {
		result.Hits = []Hit{}
		return result, nil
	}
	sqlLimit := min(MaxCandidateLimit, max(100, opts.Limit*8))
	args := pgx.NamedArgs{"q": k.q, "prefix": k.prefix, "contains": k.contains, "limit": sqlLimit}
	from, where, priority, err := hostClauses(opts, args)
	if err != nil {
		return result, err
	}
	const order = `sd.content_kind,sd.content_id,sd.content_version_id NULLS FIRST`
	cols := priority + ` AS priority,sd.terms,cardinality(sd.aliases) AS aliases`
	literal := strings.Join(k.literalRoutes(`sd.content_kind,sd.content_id,sd.content_version_id,sd.language,`+cols, from, where, "@q", "@prefix", "@contains"), " UNION ")
	queries := []string{`SELECT * FROM (` + literal + `) sd ORDER BY ` + tierSQL + ` DESC,` + order + ` LIMIT @limit`}
	if k.fuzzy() {
		queries = append(queries, fmt.Sprintf(`SELECT sd.content_kind,sd.content_id,sd.content_version_id,sd.language,%s FROM %s WHERE %s AND sd.body OPERATOR(%s.%%>) @q ORDER BY sd.body OPERATOR(%s.<->>) @q,%s LIMIT @limit`, cols, from, where, k.trgm, k.trgm, order))
	}
	// The whole request has a two-second ceiling (or an earlier caller deadline),
	// including candidate scoring. SET LOCAL applies before retrieval and cannot leak into
	// another pooled request. A SELECT CTE setting the GUC has ambiguous timing.
	if _, err = tx.Exec(ctx, `SET LOCAL pg_trgm.word_similarity_threshold=0.1; SET LOCAL statement_timeout=2000`); err != nil {
		return result, err
	}
	found := map[documentKey]Hit{}
	for _, query := range queries {
		hits, seen, err := scoreRows(ctx, tx, query, args, opts.Tenant, k)
		if err != nil {
			return result, err
		}
		for _, h := range hits {
			key := documentKey{h.Key(), h.Language}
			if old, ok := found[key]; !ok || h.Score > old.Score {
				found[key] = h
			}
		}
		if seen >= sqlLimit {
			result.Truncated = true
		}
	}
	out := make([]Hit, 0, len(found))
	for _, h := range found {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return hitLess(out[i], out[j]) })
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

// typoWindow is the nearest-distance window KeywordMatch adds typo matches from.
const typoWindow = 1000

// Match selects the content IDs a keyword query matches, as SQL a listing
// filters by and then orders and pages itself.
type Match struct {
	SQL  string
	Args pgx.NamedArgs
}

// KeywordMatch matches query in opts.ContentKinds among documents of any of
// languages: every literal match, plus the one-edit matches among the nearest
// typoWindow documents of each language. opts.Language, Limit and Eligibility
// are ignored; the listing applies its own visibility.
func KeywordMatch(ctx context.Context, pool *pgxpool.Pool, query string, opts Options, languages []string) (Match, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Match{}, err
	}
	defer tx.Rollback(ctx)
	k, err := prepareKeyword(ctx, tx, opts.Schema, query)
	if err != nil {
		return Match{}, err
	}
	if len(k.tokens) == 0 {
		return Match{SQL: `SELECT NULL::text WHERE false`}, tx.Commit(ctx)
	}
	typos := []string{}
	if k.fuzzy() {
		if _, err = tx.Exec(ctx, `SET LOCAL pg_trgm.word_similarity_threshold=0.1; SET LOCAL statement_timeout=2000`); err != nil {
			return Match{}, err
		}
		for _, language := range languages {
			o := Options{Schema: opts.Schema, Tenant: opts.Tenant, Language: language, ContentKinds: opts.ContentKinds}
			args := pgx.NamedArgs{"q": k.q, "limit": typoWindow}
			from, where, _, err := hostClauses(o, args)
			if err != nil {
				return Match{}, err
			}
			hits, _, err := scoreRows(ctx, tx, fmt.Sprintf(`SELECT sd.content_kind,sd.content_id,sd.content_version_id,sd.language,0,sd.terms,cardinality(sd.aliases) AS aliases
 FROM %s WHERE %s AND sd.body OPERATOR(%s.%%>) @q ORDER BY sd.body OPERATOR(%s.<->>) @q LIMIT @limit`, from, where, k.trgm, k.trgm), args, opts.Tenant, k)
			if err != nil {
				return Match{}, err
			}
			for _, h := range hits {
				typos = append(typos, h.ContentID)
			}
		}
	}
	qs, err := quoteIdent(opts.Schema)
	if err != nil {
		return Match{}, err
	}
	routes := k.literalRoutes("sd.content_id", qs+`.content_search_documents sd`,
		`sd.tenant_id=@k_tenant AND sd.language=ANY(@k_languages::text[]) AND sd.content_kind=ANY(@k_kinds::text[])`, "@k_q", "@k_prefix", "@k_contains")
	return Match{
		SQL: strings.Join(append(routes, `SELECT unnest(@k_typos::text[])`), " UNION ALL "),
		Args: pgx.NamedArgs{"k_tenant": opts.Tenant, "k_languages": languages, "k_kinds": opts.ContentKinds,
			"k_q": k.q, "k_prefix": k.prefix, "k_contains": k.contains, "k_typos": typos},
	}, tx.Commit(ctx)
}

// scoreRows reads one route's rows and keeps the documents keywordScore
// accepts. Rows are read before scoring so the statement finishes within its
// timeout however long scoring takes. It reports how many rows the route
// returned.
func scoreRows(ctx context.Context, tx pgx.Tx, query string, args pgx.NamedArgs, tenant string, k keywordQuery) ([]Hit, int, error) {
	type row struct {
		hit     Hit
		fields  []string
		aliases int
	}
	rows, err := tx.Query(ctx, query, args)
	if err != nil {
		return nil, 0, err
	}
	read, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		x := row{hit: Hit{ContentRef: contentref.ContentRef{TenantID: tenant}}}
		err := r.Scan(&x.hit.ContentKind, &x.hit.ContentID, &x.hit.ContentVersionID, &x.hit.Language, &x.hit.Priority, &x.fields, &x.aliases)
		return x, err
	})
	if err != nil {
		return nil, 0, err
	}
	var hits []Hit
	for _, x := range read {
		if x.hit.Score = keywordScore(ctx, k.q, k.tokens, x.fields, x.aliases); x.hit.Score > 0 {
			hits = append(hits, x.hit)
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
	}
	return hits, len(read), nil
}

func hitLess(a, b Hit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.ContentKind != b.ContentKind {
		return a.ContentKind < b.ContentKind
	}
	if a.ContentID != b.ContentID {
		return a.ContentID < b.ContentID
	}
	if a.Version() != b.Version() {
		return a.Version() < b.Version()
	}
	return a.Language < b.Language
}

// Eligible returns the candidates that exist as documents of the request tenant
// and language and pass the eligibility join, in their input
// order with Priority filled. Candidates another source proposes are never
// trusted with eligibility: this is the same join every keyword route runs.
func Eligible(ctx context.Context, pool *pgxpool.Pool, opts Options, candidates []Candidate) ([]Hit, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool is required")
	}
	if len(candidates) == 0 {
		return []Hit{}, nil
	}
	args := pgx.NamedArgs{"candidates": nil}
	from, where, priority, err := hostClauses(opts, args)
	if err != nil {
		return nil, err
	}
	type row struct {
		Kind    string  `json:"content_kind"`
		ID      string  `json:"content_id"`
		Version *string `json:"content_version_id"`
	}
	rows := make([]row, 0, len(candidates))
	byKey := map[documentKey]int{}
	for i, c := range candidates {
		if c.TenantID != opts.Tenant || c.Language != opts.Language {
			return nil, fmt.Errorf("candidate %s/%s is outside the request tenant/language", c.ContentRef, c.Language)
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		byKey[documentKey{c.Key(), c.Language}] = i
		rows = append(rows, row{c.ContentKind, c.ContentID, c.ContentVersionID})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	args["candidates"] = data
	sql := fmt.Sprintf(`SELECT sd.content_kind,sd.content_id,sd.content_version_id,%s FROM %s
 JOIN jsonb_to_recordset(@candidates::jsonb) AS c(content_kind text,content_id text,content_version_id text)
 ON c.content_kind=sd.content_kind AND c.content_id=sd.content_id AND c.content_version_id IS NOT DISTINCT FROM sd.content_version_id
 WHERE %s`, priority, from, where)
	res, err := pool.Query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	out := make([]Hit, len(candidates))
	present := make([]bool, len(candidates))
	for res.Next() {
		ref := contentref.ContentRef{TenantID: opts.Tenant}
		var pr int32
		if err := res.Scan(&ref.ContentKind, &ref.ContentID, &ref.ContentVersionID, &pr); err != nil {
			return nil, err
		}
		i, ok := byKey[documentKey{ref.Key(), opts.Language}]
		if !ok {
			continue
		}
		c := candidates[i]
		out[i], present[i] = Hit{ContentRef: c.ContentRef, Language: c.Language, Priority: pr, Score: c.Score}, true
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	kept := make([]Hit, 0, len(out))
	for i, h := range out {
		if present[i] {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

func keywordTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) })
}

func keywordScore(ctx context.Context, q string, tokens, fields []string, aliases int) float32 {
	if len(fields) == 0 || q == "" {
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
	if strings.HasPrefix(fields[0], q) {
		return .85
	}
	for i := 1; i < len(fields) && i <= aliases; i++ {
		if strings.HasPrefix(fields[i], q) {
			return .84
		}
	}
	if strings.Contains(fields[0], q) {
		return .82
	}
	for i := 1; i < len(fields) && i <= aliases; i++ {
		if strings.Contains(fields[i], q) {
			return .81
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
