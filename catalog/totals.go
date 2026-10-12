package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/cache"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
)

var (
	// Store.Totals: one count per site, reloaded in the background every ten
	// minutes; the last good count serves for a day.
	totalsPolicy = cache.Options{Name: "contentkit.catalog_totals", RefreshAfter: 10 * time.Minute, MaxStale: 24 * time.Hour, LoadTimeout: 30 * time.Second}
	// TotalsClient: a sibling deployment's totals, which its route lets any
	// cache keep for ten minutes.
	remoteTotalsPolicy = cache.Options{Name: "contentkit.remote_catalog_totals", RefreshAfter: 10 * time.Minute, MaxStale: 24 * time.Hour}
)

// TotalsQuery selects the kinds and languages to total.
type TotalsQuery struct {
	// Kinds are content kinds; empty means all of them.
	Kinds []string
	// Languages are the languages Total.Languages reports; empty means every
	// language with a counted item. Kind totals ignore it.
	Languages []string
	// Site is the site whose scope applies (its name); empty is the first
	// site. The HTTP route takes it from the request's host.
	Site string
}

// Total sizes one kind. Public counts what an anonymous visitor's listing
// shows and opens; Tiers[t] every eligible item a holder of t opens,
// members-listed ones included. An item counts once in the kind's total and
// once per language any of its eligible versions serves, never per version.
// The site's scope and the default content filters apply.
type Total struct {
	Public    int64                    `json:"public"`
	Tiers     map[string]int64         `json:"tiers"`
	Languages map[string]LanguageTotal `json:"languages"`
}

// LanguageTotal sizes one kind in one language, as Total does.
type LanguageTotal struct {
	Public int64            `json:"public"`
	Tiers  map[string]int64 `json:"tiers"`
}

// Totals is the catalog's size by content kind.
type Totals struct {
	Kinds map[string]Total `json:"kinds"`
}

type totalsKey struct{ site, kind string }

// Totals counts the listed content of each kind by who opens it, cached per
// site and kind: a count reloads in the background after ten minutes, and
// the last good one serves while reloads fail, for up to a day.
func (s *Store) Totals(ctx context.Context, q TotalsQuery) (Totals, error) {
	out := Totals{Kinds: map[string]Total{}}
	site, err := s.site(q.Site)
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
		out.Kinds[k.kind] = got[k].only(languages, s.cfg.Tiers)
	}
	return out, nil
}

func (s *Store) site(name string) (kinds.Site, error) {
	if name == "" {
		site, _ := s.cfg.SiteFor("")
		return site, nil
	}
	for _, site := range s.cfg.Sites {
		if site.Name == name {
			return site, nil
		}
	}
	return kinds.Site{}, invalid("unknown site %q", name)
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
func (t Total) only(languages, tiers []string) Total {
	if len(languages) == 0 {
		languages = slices.Collect(maps.Keys(t.Languages))
	}
	out := Total{Public: t.Public, Tiers: maps.Clone(t.Tiers), Languages: make(map[string]LanguageTotal, len(languages))}
	if out.Tiers == nil {
		out.Tiers = zeroTiers(tiers)
	}
	for _, l := range languages {
		lt, ok := t.Languages[l]
		if !ok {
			lt = LanguageTotal{Tiers: zeroTiers(tiers)}
		}
		out.Languages[l] = LanguageTotal{Public: lt.Public, Tiers: maps.Clone(lt.Tiers)}
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
func (s *Store) loadTotals(ctx context.Context, keys []totalsKey) (map[totalsKey]Total, error) {
	bySite := map[string][]string{}
	for _, k := range keys {
		if !slices.Contains(bySite[k.site], k.kind) {
			bySite[k.site] = append(bySite[k.site], k.kind)
		}
	}
	out := make(map[totalsKey]Total, len(keys))
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
// filter groups. Audiences are access.AudiencesSQL's: a free row opens to
// anyone and every tier, a tier row to its tiers, others to nobody. Versions collapse to
// their content item, once per language and once per kind.
func (s *Store) countTotals(ctx context.Context, site kinds.Site, kindNames []string) (map[string]Total, error) {
	refs := append([]kinds.GroupRef{}, site.RequireGroups...)
	for _, f := range s.cfg.ContentFilters {
		if !f.DefaultVisible {
			refs = append(refs, f.Group)
		}
	}
	ids, err := s.groupIDs(ctx, refs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Total, len(kindNames))
	for _, k := range kindNames {
		out[k] = Total{Tiers: zeroTiers(s.cfg.Tiers), Languages: map[string]LanguageTotal{}}
	}
	require, hidden := []string{}, []string{}
	for _, r := range site.RequireGroups {
		id, ok := ids[r]
		if !ok {
			return out, nil // a required group that does not exist admits nothing
		}
		require = append(require, id)
	}
	for _, f := range s.cfg.ContentFilters {
		if id, ok := ids[f.Group]; ok && !f.DefaultVisible {
			hidden = append(hidden, id)
		}
	}
	audiences, args, err := access.AudiencesSQL("f", s.accessG.Tiers())
	if err != nil {
		return nil, err
	}
	named := pgx.NamedArgs{"tenant": s.tenant, "kinds": kindNames, "now": s.now(), "exclude": orEmpty(site.ExcludeRestrictions),
		"require": require, "hidden": hidden}
	for k, v := range args {
		named[k] = v
	}
	rows, err := s.pool.Query(ctx, `SELECT a.kind, coalesce(a.language, ''), GROUPING(a.language) = 1, a.audience, count(DISTINCT a.content_id) FROM (
 SELECT DISTINCT f.kind, f.content_id, l.language, au.audience FROM `+s.t.facets+` f
 CROSS JOIN LATERAL unnest(f.languages) AS l(language)
 CROSS JOIN LATERAL unnest(`+audiences+`) AS au(audience)
 WHERE f.tenant_id = @tenant AND f.kind = ANY(@kinds) AND NOT f.deleted AND f.live_from <= @now AND f.listing IN ('listed', 'members')
  AND NOT (f.restrictions && @exclude::text[]) AND f.group_ids @> @require::text[] AND NOT (f.group_ids && @hidden::text[])
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
			lt = LanguageTotal{Tiers: zeroTiers(s.cfg.Tiers)}
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

// TotalsClient reads another deployment's catalog totals (GET
// /v1/catalog/totals), such as a sibling site's, server to server, cached
// per kind like Store.Totals.
type TotalsClient struct {
	url    string
	http   *http.Client
	totals *cache.Cache[string, Total]
}

// NewTotalsClient reads the deployment whose ContentKit is mounted at baseURL,
// e.g. "https://hentai0.com/api/v1/contentkit". A nil hc has a ten-second
// timeout.
func NewTotalsClient(baseURL string, hc *http.Client) (*TotalsClient, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, invalid("TotalsClient base URL %q must be absolute http(s)", baseURL)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	c := &TotalsClient{url: u.String() + totalsPath, http: hc}
	if c.totals, err = cache.New(remoteTotalsPolicy, c.load); err != nil {
		return nil, err
	}
	return c, nil
}

// Close releases the cache's goroutines.
func (c *TotalsClient) Close() { c.totals.Close() }

// Totals reads the remote's totals as Store.Totals answers them, in the site
// the remote derives from its host: q.Kinds is required and q.Site empty.
func (c *TotalsClient) Totals(ctx context.Context, q TotalsQuery) (Totals, error) {
	out := Totals{Kinds: map[string]Total{}}
	if len(q.Kinds) == 0 || q.Site != "" {
		return out, invalid("TotalsClient needs kinds and no site")
	}
	languages := []string{}
	for _, l := range q.Languages {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(languages, l) {
			languages = append(languages, l)
		}
	}
	got, err := c.totals.Get(ctx, q.Kinds)
	if err != nil {
		return out, err
	}
	for _, k := range q.Kinds {
		t, ok := got[k]
		if !ok {
			return out, fmt.Errorf("catalog: %s answered no total for %q", c.url, k)
		}
		out.Kinds[k] = t.only(languages, slices.Sorted(maps.Keys(t.Tiers)))
	}
	return out, nil
}

func (c *TotalsClient) load(ctx context.Context, kinds []string) (map[string]Total, error) {
	kinds = slices.Compact(slices.Sorted(slices.Values(kinds)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"?kinds="+url.QueryEscape(strings.Join(kinds, ",")), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog: totals: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("catalog: totals: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		_ = json.Unmarshal(body, &e)
		return nil, fmt.Errorf("catalog: totals: %s answered %d %s", c.url, resp.StatusCode, e.Code)
	}
	var t Totals
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("catalog: totals: %w", err)
	}
	return t.Kinds, nil
}
