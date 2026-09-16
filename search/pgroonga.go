package search

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Discover against the current connection pool. This small catalog lookup avoids
// process-global pool retention, stale schema ownership and cached startup errors.
func getPGroongaExtensionSchema(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("pool is required")
	}

	var schema string
	err := pool.QueryRow(ctx, `
			SELECT n.nspname
			FROM pg_extension e
			JOIN pg_namespace n ON n.oid = e.extnamespace
			WHERE e.extname = 'pgroonga'
		`).Scan(&schema)
	if err != nil {
		// Do not cache failures: a transient connection or migration error must
		// not poison every future PGroonga search in this process.
		return "", fmt.Errorf("detect pgroonga extension schema: %w", err)
	}

	return schema, nil
}

// PGroongaHit is a lexical hit returned by PGroonga-backed search.
//
// Score is normalized (target: [0..1]) so it can be blended with other lexical
// backends (FTS/trigram) without per-language tuning in host apps.
type PGroongaHit struct {
	EntityType string
	EntityID   string
	Language   string
	Score      float32 // normalized
	RawScore   float32 // backend raw score (for debugging/calibration)
}

type PGroongaOptions struct {
	Schema      string
	Language    string
	EntityTypes []string
	Limit       int

	// Prefix enables a loose "typeahead" mode where each whitespace-delimited
	// token is treated as a prefix.
	Prefix bool

	// ScoreK controls normalization: normalized = raw / (raw + ScoreK).
	// Defaults to 1.
	ScoreK float32

	// FilterSQL is an optional additional WHERE fragment appended to the query as:
	//   ... AND (<FilterSQL>)
	//
	// It is intended for host-owned constraints that must be enforced inside the
	// retrieval query.
	//
	// IMPORTANT: this is trusted SQL provided by the host app. Do not insert
	// user input into it unsafely.
	FilterSQL string
	// FilterArgs are named args referenced by FilterSQL using pgx '@name'
	// placeholders (e.g. "... language = @lang").
	FilterArgs map[string]any
}

// NormalizePGroongaScore converts a raw PGroonga score into a [0..1] range
// suitable for blending with other lexical backends.
//
// Normalization uses: raw / (raw + k). k defaults to 1.
func NormalizePGroongaScore(raw float32, k float32) float32 {
	if raw <= 0 {
		return 0
	}
	if k <= 0 {
		k = 1
	}
	return raw / (raw + k)
}

func sanitizePGroongaQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}

	// We intentionally avoid PGroonga's query parser features and only keep
	// "word-ish" characters. This prevents special query syntax from being
	// interpreted and keeps queries cheap/deterministic.
	var b strings.Builder
	b.Grow(len(q))
	lastSpace := false
	for _, r := range q {
		keep := unicode.IsLetter(r) || unicode.IsNumber(r)
		if keep {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		// Treat everything else as a separator.
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

func buildPGroongaTypeaheadQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	toks := strings.Fields(q)
	if len(toks) == 0 {
		return ""
	}
	for i := range toks {
		// Basic prefix marker. PGroonga supports query syntax; '*' commonly
		// indicates prefix in its query parser.
		if !strings.HasSuffix(toks[i], "*") {
			toks[i] = toks[i] + "*"
		}
	}
	return strings.Join(toks, " ")
}

func buildPGroongaSQL(docSchema string, extSchema string, entityTypes []string, filterSQL string, filterArgs map[string]any) (string, pgx.NamedArgs, string, error) {
	qs, err := quoteIdent(docSchema)
	if err != nil {
		return "", nil, "", fmt.Errorf("invalid schema: %w", err)
	}
	table := qs + ".search_documents"

	qext, err := quoteIdent(extSchema)
	if err != nil {
		return "", nil, "", fmt.Errorf("invalid pgroonga schema: %w", err)
	}

	where := "WHERE sd.language = @language AND sd.raw_document IS NOT NULL AND btrim(sd.raw_document) <> ''"
	args := pgx.NamedArgs{
		"language": "",
		"q":        "",
		"limit":    0,
	}
	if len(entityTypes) > 0 {
		where += " AND sd.entity_type = ANY(@entity_types::text[])"
		args["entity_types"] = entityTypes
	}
	if strings.TrimSpace(filterSQL) != "" {
		where += " AND (" + filterSQL + ")"
		if err := mergeNamedArgs(args, filterArgs); err != nil {
			return "", nil, "", err
		}
	}

	// Query uses PGroonga's query syntax operator. Hosts must ensure the
	// extension is installed and indexes exist; otherwise this will error.
	sql := fmt.Sprintf(`
		SELECT
			sd.entity_type,
			sd.entity_id,
			sd.language,
			%[1]s.pgroonga_score(tableoid, ctid)::float4 AS raw_score
		FROM %s sd
		%s
		  AND sd.raw_document OPERATOR(%[1]s.&@~) @q
		ORDER BY raw_score DESC, sd.entity_type ASC, sd.entity_id ASC
		LIMIT @limit
	`, qext, table, where)

	return sql, args, table, nil
}

// PGroongaSearch runs a PGroonga-backed lexical search against
// `<schema>.search_documents.raw_document`.
//
// This is intended for languages like ja/zh/ko where Postgres FTS tokenization
// is insufficient and trigram transliteration is lossy.
func PGroongaSearch(ctx context.Context, pool *pgxpool.Pool, query string, opts PGroongaOptions) ([]PGroongaHit, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool is required")
	}
	if strings.TrimSpace(opts.Schema) == "" {
		return nil, fmt.Errorf("schema is required")
	}
	if strings.TrimSpace(opts.Language) == "" {
		return nil, fmt.Errorf("language is required")
	}
	if opts.Limit <= 0 {
		return []PGroongaHit{}, nil
	}

	q := strings.TrimSpace(query)
	q = strings.Join(strings.Fields(q), " ")
	if q == "" {
		return []PGroongaHit{}, nil
	}

	q = sanitizePGroongaQuery(q)
	if q == "" {
		return []PGroongaHit{}, nil
	}
	if opts.Prefix {
		q = buildPGroongaTypeaheadQuery(q)
		if q == "" {
			return []PGroongaHit{}, nil
		}
	}

	extSchema, err := getPGroongaExtensionSchema(ctx, pool)
	if err != nil {
		return nil, err
	}

	sql, args, _, err := buildPGroongaSQL(opts.Schema, extSchema, opts.EntityTypes, opts.FilterSQL, opts.FilterArgs)
	if err != nil {
		return nil, err
	}
	args["language"] = opts.Language
	args["q"] = q
	args["limit"] = opts.Limit

	rows, err := pool.Query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	k := opts.ScoreK
	if k <= 0 {
		k = 1
	}

	var out []PGroongaHit
	for rows.Next() {
		var h PGroongaHit
		if err := rows.Scan(&h.EntityType, &h.EntityID, &h.Language, &h.RawScore); err != nil {
			return nil, err
		}
		h.Score = NormalizePGroongaScore(h.RawScore, k)
		out = append(out, h)
	}
	return out, rows.Err()
}
