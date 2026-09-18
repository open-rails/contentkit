// Package search is ContentKit's keyword retrieval over content_search_documents:
// exact names and aliases, native-script prefixes and bounded typos for every
// language, with the host's eligibility join applied inside every route.
package search

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// MaxCandidateLimit bounds one route's document window.
const MaxCandidateLimit = 10_000

// Eligibility is trusted host SQL joined laterally to every candidate document
// (alias sd: tenant_id, content_kind, content_id, content_version_id, language).
// It returns no row when the document is not eligible for this request, or one
// row with the column priority (integer: preferred document among a content
// item's equal matches, lower first). Ownership, access, publication and every
// requested version trait must hold on that one row; sibling documents never
// make each other eligible.
type Eligibility struct {
	SQL  string
	Args map[string]any
}

// Options selects one tenant, language and window of a keyword request.
type Options struct {
	Schema   string
	Tenant   string
	Language string
	// ContentKinds limits the kinds searched; empty means every kind.
	ContentKinds []string
	Limit        int

	// FilterSQL is trusted host SQL appended as `AND (<FilterSQL>)` on sd;
	// FilterArgs binds its pgx '@name' placeholders. Reserved names: tenant,
	// language, q, prefix, limit, kinds, candidates.
	FilterSQL  string
	FilterArgs map[string]any

	Eligibility *Eligibility
}

// Hit is one matched document.
type Hit struct {
	contentref.ContentRef
	Language string
	// Priority orders same-item documents at equal match; lower is preferred.
	Priority int32
	Score    float32
}

// Result is one language's scored document window.
type Result struct {
	// Hits are documents ordered by score, content kind, content id and version.
	Hits []Hit
	// Truncated reports that a route filled its SQL window or that more scored
	// documents existed than Limit: documents beyond the window were never ranked.
	Truncated bool
}

// Candidate is one document proposed by another retrieval source.
type Candidate struct {
	contentref.ContentRef
	Language string
	Score    float32
}

func quoteIdent(ident string) (string, error) {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return "", fmt.Errorf("empty identifier")
	}
	for _, r := range ident {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return "", fmt.Errorf("invalid identifier %q", ident)
	}
	return `"` + ident + `"`, nil
}

// QuoteSchema validates and quotes a schema identifier for embedding in SQL.
func QuoteSchema(schema string) (string, error) { return quoteIdent(schema) }

func mergeNamedArgs(dst pgx.NamedArgs, extra map[string]any) error {
	for k, v := range extra {
		k = strings.TrimSpace(k)
		if k == "" {
			return fmt.Errorf("empty FilterArgs key")
		}
		if _, exists := dst[k]; exists {
			return fmt.Errorf("FilterArgs key %q conflicts with reserved arg", k)
		}
		dst[k] = v
	}
	return nil
}

// hostClauses renders the tenant/language/kind filters, host filter and the
// eligibility join shared by every route.
func hostClauses(opts Options, args pgx.NamedArgs) (from, where, priority string, err error) {
	qs, err := quoteIdent(opts.Schema)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(opts.Tenant) == "" || strings.TrimSpace(opts.Language) == "" {
		return "", "", "", fmt.Errorf("tenant and language are required")
	}
	args["tenant"], args["language"] = opts.Tenant, opts.Language
	where = `sd.tenant_id=@tenant AND sd.language=@language`
	if len(opts.ContentKinds) > 0 {
		where += ` AND sd.content_kind=ANY(@kinds::text[])`
		args["kinds"] = opts.ContentKinds
	}
	if strings.TrimSpace(opts.FilterSQL) != "" {
		where += ` AND (` + opts.FilterSQL + `)`
		if err := mergeNamedArgs(args, opts.FilterArgs); err != nil {
			return "", "", "", err
		}
	}
	join, priority, err := EligibilityJoin(opts.Eligibility, args)
	if err != nil {
		return "", "", "", err
	}
	return qs + `.content_search_documents sd` + join, where, priority, nil
}

// EligibilityJoin renders the host eligibility query as the lateral join every
// route applies to a candidate row aliased sd, binding its named args. It
// returns the join clause (empty without eligibility) and the priority
// expression to select.
func EligibilityJoin(e *Eligibility, args pgx.NamedArgs) (join, priority string, err error) {
	if e == nil || strings.TrimSpace(e.SQL) == "" {
		return "", "0::int", nil
	}
	if err := mergeNamedArgs(args, e.Args); err != nil {
		return "", "", err
	}
	// LATERAL binds sd inside the host query; a missing column fails loudly.
	return ` JOIN LATERAL (SELECT h.priority::int AS priority FROM (` + e.SQL + `) AS h LIMIT 1) e ON true`, `e.priority`, nil
}
