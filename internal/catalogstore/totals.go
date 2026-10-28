package catalogstore

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	catalog "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/helpers/cache"
)

var (
	// Store.Totals: one count per site, reloaded in the background every ten
	// minutes; the last good count serves for a day.
	totalsPolicy = cache.Options{Name: "contentkit.catalog_totals", RefreshAfter: 10 * time.Minute, MaxStale: 24 * time.Hour, LoadTimeout: 30 * time.Second}
)

type totalsKey struct{ site, kind string }

// Totals counts the listed content of each kind by who opens it, cached per
// site and kind: a count reloads in the background after ten minutes, and
// the last good one serves while reloads fail, for up to a day.
func (s *Store) Totals(ctx context.Context, q catalog.TotalsQuery) (catalog.Totals, error) {
	out := catalog.Totals{Kinds: map[string]catalog.Total{}}
	site, err := s.site(q.Domain)
	if err != nil {
		return out, err
	}
	languages, err := s.totalsLanguages(q.Languages)
	if err != nil {
		return out, err
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = slices.Sorted(maps.Keys(s.cfg.Kinds))
	}
	keys := make([]totalsKey, 0, len(kinds))
	for _, k := range kinds {
		if _, ok := s.cfg.Kinds[k]; !ok {
			return out, invalid("unknown kind %q", k)
		}
		keys = append(keys, totalsKey{site.Name, k})
	}
	got, err := s.totals.Get(ctx, keys)
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		out.Kinds[k.kind] = totalOnly(got[k], languages, s.cfg.Tiers)
	}
	return out, nil
}

func (s *Store) site(name string) (kinds.Domain, error) {
	if name == "" {
		site, _ := s.cfg.DomainFor("")
		return site, nil
	}
	for _, site := range s.cfg.ContentFilters.Domains {
		if site.Name == name {
			return site, nil
		}
	}
	return kinds.Domain{}, invalid("unknown site %q", name)
}

func (s *Store) totalsLanguages(in []string) ([]string, error) {
	out := []string{}
	for _, l := range in {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if err := s.checkLanguage(l); err != nil {
			return nil, err
		}
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// only copies t with the given languages, zero where nothing counts; none
// keeps all. Copies keep callers off the cached maps.
func totalOnly(t catalog.Total, languages, tiers []string) catalog.Total {
	if len(languages) == 0 {
		languages = slices.Collect(maps.Keys(t.Languages))
	}
	out := catalog.Total{Public: t.Public, Tiers: maps.Clone(t.Tiers), Languages: make(map[string]catalog.LanguageTotal, len(languages))}
	if out.Tiers == nil {
		out.Tiers = zeroTiers(tiers)
	}
	for _, l := range languages {
		lt, ok := t.Languages[l]
		if !ok {
			lt = catalog.LanguageTotal{Tiers: zeroTiers(tiers)}
		}
		out.Languages[l] = catalog.LanguageTotal{Public: lt.Public, Tiers: maps.Clone(lt.Tiers)}
	}
	return out
}

func zeroTiers(tiers []string) map[string]int64 {
	m := make(map[string]int64, len(tiers))
	for _, t := range tiers {
		m[t] = 0
	}
	return m
}

// loadTotals counts keys, one query per site.
func (s *Store) loadTotals(ctx context.Context, keys []totalsKey) (map[totalsKey]catalog.Total, error) {
	bySite := map[string][]string{}
	for _, k := range keys {
		if !slices.Contains(bySite[k.site], k.kind) {
			bySite[k.site] = append(bySite[k.site], k.kind)
		}
	}
	out := make(map[totalsKey]catalog.Total, len(keys))
	for _, name := range slices.Sorted(maps.Keys(bySite)) {
		site, err := s.site(name)
		if err != nil {
			return nil, err
		}
		got, err := s.countTotals(ctx, site, bySite[name])
		if err != nil {
			return nil, err
		}
		for kind, t := range got {
			out[totalsKey{name, kind}] = t
		}
	}
	return out, nil
}

// countTotals counts over content_facets: live, not deleted, listed or
// members-listed versions in the site's scope outside the default-hidden
// filter entities. Audiences are access.AudiencesSQL's: a free row opens to
// anyone and every tier, a tier row to its tiers, others to nobody. Versions collapse to
// their content item, once per language and once per kind.
func (s *Store) countTotals(ctx context.Context, site kinds.Domain, kindNames []string) (map[string]catalog.Total, error) {
	policies, err := domainpolicy.Resolve(ctx, s.pool, s.t.entities, s.tenant, site, s.cfg.ContentFilters.AppliesTo)
	if err != nil {
		return nil, err
	}
	out := make(map[string]catalog.Total, len(kindNames))
	for _, k := range kindNames {
		out[k] = catalog.Total{Tiers: zeroTiers(s.cfg.Tiers), Languages: map[string]catalog.LanguageTotal{}}
	}
	hidden := map[string][]string{}
	for kind, p := range policies {
		for id, visible := range p.Toggles {
			if !visible {
				hidden[kind] = append(hidden[kind], id)
			}
		}
	}
	hard, hardArgs := policies.SQL("f", "totals_domain")
	soft, softArgs := domainpolicy.HiddenSQL(hidden, "f", "totals_hidden")
	audiences, args, err := entitlement.AudiencesSQL("f", s.accessG.Tiers())
	if err != nil {
		return nil, err
	}
	named := pgx.NamedArgs{"tenant": s.tenant, "kinds": kindNames, "now": s.now()}
	for k, v := range hardArgs {
		named[k] = v
	}
	for k, v := range softArgs {
		named[k] = v
	}
	for k, v := range args {
		named[k] = v
	}
	rows, err := s.pool.Query(ctx, `SELECT a.kind, coalesce(a.language, ''), GROUPING(a.language) = 1, a.audience, count(DISTINCT a.content_id) FROM (
 SELECT DISTINCT f.kind, f.content_id, l.language, au.audience FROM `+s.t.facets+` f
 CROSS JOIN LATERAL unnest(f.languages) AS l(language)
 CROSS JOIN LATERAL unnest(`+audiences+`) AS au(audience)
 WHERE f.tenant_id = @tenant AND f.kind = ANY(@kinds) AND NOT f.deleted AND f.live_from <= @now
  AND `+hard+` AND `+soft+`
) a GROUP BY GROUPING SETS ((a.kind, a.audience, a.language), (a.kind, a.audience))`, named)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, language, audience string
		var all bool
		var n int64
		if err := rows.Scan(&kind, &language, &all, &audience, &n); err != nil {
			return nil, err
		}
		t := out[kind]
		if all {
			if audience == "" {
				t.Public = n
			} else {
				t.Tiers[audience] = n
			}
			out[kind] = t
			continue
		}
		lt, ok := t.Languages[language]
		if !ok {
			lt = catalog.LanguageTotal{Tiers: zeroTiers(s.cfg.Tiers)}
		}
		if audience == "" {
			lt.Public = n
		} else {
			lt.Tiers[audience] = n
		}
		t.Languages[language] = lt
	}
	return out, rows.Err()
}
