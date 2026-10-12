package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/cache"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/search"
)

// ContentKit's catalog-totals cache policy, in one place.
var (
	// Store.Totals: one COUNT over every listed document, reloaded in the
	// background every ten minutes; the last good count serves for a day.
	totalsPolicy = cache.Options{Name: "contentkit.catalog_totals", RefreshAfter: 10 * time.Minute, MaxStale: 24 * time.Hour, LoadTimeout: 30 * time.Second}
	// TotalsClient: a sibling deployment's totals, which its route lets any
	// cache keep for ten minutes.
	remoteTotalsPolicy = cache.Options{Name: "contentkit.remote_catalog_totals", RefreshAfter: 10 * time.Minute, MaxStale: 24 * time.Hour}
)

// Listing is the public catalog Totals sizes: the works an anonymous listing
// shows, and who opens each.
type Listing struct {
	// Kinds are the content kinds counted; Totals answers no other.
	Kinds []string
	// Eligibility is trusted host SQL joined laterally to each document sd, as
	// search.Eligibility, but it admits only what a listing shows: no row unless
	// the version is listed (live, not deleted, not unlisted, not hidden on this
	// deployment), else one row Access reads as h. It is not CountEligibility,
	// which also admits what listings hide. Reserved arg names: tenant, kinds,
	// any_of_kinds, any_of_slugs, none_of_kinds, none_of_slugs and Access's.
	Eligibility search.Eligibility
	// Access maps h to its access level: Level, Levels and Tier or TierName,
	// expressions over h. ID, Kind and Scope are unused.
	Access access.Columns
	// Tiers are the tiers Totals reports; with Access.TierName it defaults to
	// that one.
	Tiers []string
}

// listing is a validated Listing with its compiled audience expression.
type listing struct {
	kinds     []string
	tiers     []string
	sql       string
	audiences string
	args      map[string]any
}

func (s *Store) compileListing(l *Listing) (*listing, error) {
	out := &listing{sql: strings.TrimSpace(l.Eligibility.SQL), tiers: slices.Clone(l.Tiers)}
	if out.sql == "" {
		return nil, fmt.Errorf("%w: Listing.Eligibility.SQL is required", ErrInvalid)
	}
	for _, k := range l.Kinds {
		k = strings.TrimSpace(k)
		if !identRE.MatchString(k) || s.isKind(k) {
			return nil, fmt.Errorf("%w: Listing kind %q must be a host content kind matching %s", ErrInvalid, k, identRE)
		}
		if !slices.Contains(out.kinds, k) {
			out.kinds = append(out.kinds, k)
		}
	}
	if len(out.kinds) == 0 {
		return nil, fmt.Errorf("%w: Listing.Kinds is required", ErrInvalid)
	}
	if len(out.tiers) == 0 && l.Access.TierName != "" {
		out.tiers = []string{l.Access.TierName}
	}
	expr, args, err := l.Access.Audiences(out.tiers)
	if err != nil {
		return nil, fmt.Errorf("%w: Listing.Access: %v", ErrInvalid, err)
	}
	out.audiences = expr
	reserved := []string{"tenant", "kinds", "any_of_kinds", "any_of_slugs", "none_of_kinds", "none_of_slugs"}
	for k, v := range l.Eligibility.Args {
		if _, dup := args[k]; dup || slices.Contains(reserved, k) {
			return nil, fmt.Errorf("%w: Listing.Eligibility arg %q is reserved", ErrInvalid, k)
		}
		args[k] = v
	}
	out.args = args
	return out, nil
}

// NodeKey names an active node by kind and slug.
type NodeKey struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
}

// CatalogScope narrows totals to one site's slice of the catalog by the nodes
// effective on each counted version: its work's and its own active
// assignments of active nodes, as listings filter by tags.
type CatalogScope struct {
	// AnyOf keeps versions with at least one of these; empty keeps all.
	AnyOf []NodeKey `json:"any_of,omitempty"`
	// NoneOf drops versions with any of these.
	NoneOf []NodeKey `json:"none_of,omitempty"`
}

// scopeKey is the scope's canonical form: validated, sorted, deduplicated.
func (s *Store) scopeKey(sc CatalogScope) (string, error) {
	norm := func(keys []NodeKey) ([]NodeKey, error) {
		out := make([]NodeKey, 0, len(keys))
		for _, k := range keys {
			if err := s.requireKind(k.Kind); err != nil {
				return nil, err
			}
			slug, err := validateSlug(k.Slug)
			if err != nil {
				return nil, err
			}
			out = append(out, NodeKey{k.Kind, slug})
		}
		slices.SortFunc(out, func(a, b NodeKey) int { return strings.Compare(a.Kind+"/"+a.Slug, b.Kind+"/"+b.Slug) })
		return slices.Compact(out), nil
	}
	var err error
	if sc.AnyOf, err = norm(sc.AnyOf); err != nil {
		return "", err
	}
	if sc.NoneOf, err = norm(sc.NoneOf); err != nil {
		return "", err
	}
	b, err := json.Marshal(sc)
	return string(b), err
}

// TotalsQuery selects the kinds to total and the site scope to total them in.
type TotalsQuery struct {
	// Kinds are Listing kinds; empty means all of them.
	Kinds []string
	Scope CatalogScope
}

// Total sizes one kind's listing: Public works anyone opens, and per tier
// every work its holders open, public ones included. A work counts once
// whatever its languages and versions.
type Total struct {
	Public int64            `json:"public"`
	Tiers  map[string]int64 `json:"tiers"`
}

// Totals is the catalog's size by content kind.
type Totals struct {
	Kinds map[string]Total `json:"kinds"`
}

// totalsKey is one kind under one canonical scope: the unit totals load and cache.
type totalsKey struct{ kind, scope string }

// Totals counts the listed works of each kind by who opens them, cached per
// kind and scope: a count is reloaded in the background after ten minutes and
// the last good one served while reloads fail, for up to a day. It needs
// Options.Listing.
func (s *Store) Totals(ctx context.Context, q TotalsQuery) (Totals, error) {
	out := Totals{Kinds: map[string]Total{}}
	if s.listing == nil {
		return out, fmt.Errorf("%w: Options.Listing is not set", ErrInvalid)
	}
	scope, err := s.scopeKey(q.Scope)
	if err != nil {
		return out, err
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = s.listing.kinds
	}
	keys := make([]totalsKey, 0, len(kinds))
	for _, k := range kinds {
		if !slices.Contains(s.listing.kinds, k) {
			return out, fmt.Errorf("%w: kind %q is not listed", ErrInvalid, k)
		}
		keys = append(keys, totalsKey{k, scope})
	}
	got, err := s.totals.Get(ctx, keys)
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		out.Kinds[k.kind] = got[k]
	}
	return out, nil
}

// Close releases the totals cache's goroutines; call it when discarding a
// store with a Listing.
func (s *Store) Close() {
	if s.totals != nil {
		s.totals.Close()
	}
}

// loadTotals counts keys, one query per scope.
func (s *Store) loadTotals(ctx context.Context, keys []totalsKey) (map[totalsKey]Total, error) {
	byScope := map[string][]string{}
	for _, k := range keys {
		if !slices.Contains(byScope[k.scope], k.kind) {
			byScope[k.scope] = append(byScope[k.scope], k.kind)
		}
	}
	out := make(map[totalsKey]Total, len(keys))
	for _, scope := range slices.Sorted(maps.Keys(byScope)) {
		var sc CatalogScope
		if err := json.Unmarshal([]byte(scope), &sc); err != nil {
			return nil, err
		}
		got, err := s.countTotals(ctx, sc, byScope[scope])
		if err != nil {
			return nil, err
		}
		for kind, t := range got {
			out[totalsKey{kind, scope}] = t
		}
	}
	return out, nil
}

func (s *Store) countTotals(ctx context.Context, sc CatalogScope, kinds []string) (map[string]Total, error) {
	l := s.listing
	args := pgx.NamedArgs{"tenant": s.tenant, "kinds": kinds}
	maps.Copy(args, l.args)
	where := ""
	for _, f := range []struct {
		name, op string
		keys     []NodeKey
	}{{"any_of", "EXISTS", sc.AnyOf}, {"none_of", "NOT EXISTS", sc.NoneOf}} {
		if len(f.keys) == 0 {
			continue
		}
		ks, ss := make([]string, len(f.keys)), make([]string, len(f.keys))
		for i, k := range f.keys {
			ks[i], ss[i] = k.Kind, k.Slug
		}
		args[f.name+"_kinds"], args[f.name+"_slugs"] = ks, ss
		where += fmt.Sprintf(`
   AND %[1]s (SELECT 1 FROM %[2]s.content_assignments a
     WHERE a.tenant_id=sd.tenant_id AND a.content_kind=sd.content_kind AND a.content_id=sd.content_id
       AND coalesce(a.content_version_id,'') IN ('', sd.content_version_id) AND a.state='active'
       AND a.taxonomy_id IN (SELECT n.taxonomy_id FROM %[2]s.content_nodes n
         JOIN unnest(@%[3]s_kinds::text[], @%[3]s_slugs::text[]) AS k(kind, slug) ON k.kind=n.kind AND k.slug=n.slug
         WHERE n.tenant_id=@tenant AND n.state='active'))`, f.op, s.qs, f.name)
	}
	out := make(map[string]Total, len(kinds))
	for _, k := range kinds {
		t := Total{Tiers: make(map[string]int64, len(l.tiers))}
		for _, tier := range l.tiers {
			t.Tiers[tier] = 0
		}
		out[k] = t
	}
	// One row per work and audience: languages and versions collapse to the work.
	err := s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT w.content_kind, w.audience, count(*) FROM (
 SELECT DISTINCT sd.content_kind, sd.content_id, au.audience
 FROM %s.content_search_documents sd
 JOIN LATERAL (SELECT %s AS audiences FROM (%s) AS h LIMIT 1) e ON true
 CROSS JOIN LATERAL unnest(e.audiences) AS au(audience)
 WHERE sd.tenant_id=@tenant AND sd.content_kind=ANY(@kinds::text[])%s
) w GROUP BY w.content_kind, w.audience`, s.qs, l.audiences, l.sql, where), args)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, audience string
			var n int64
			if err := rows.Scan(&kind, &audience, &n); err != nil {
				return err
			}
			t := out[kind]
			if audience == access.Anyone {
				t.Public = n
			} else {
				t.Tiers[audience] = n
			}
			out[kind] = t
		}
		return rows.Err()
	})
	return out, err
}
