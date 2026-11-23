// Package locationpolicy applies request-local location exclusions. Rules are
// read from Postgres on every request; only the GeoIP file reader is retained.
package locationpolicy

import (
	"context"
	"net/netip"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/content"
)

func DomainName(name string) string {
	if name == "" {
		return "main"
	}
	return name
}

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type Block struct {
	Site       bool
	ContentIDs []string
	EntityIDs  []string
	Matches    []content.LocationRuleMatch
}

func (b Block) Empty() bool { return !b.Site && len(b.ContentIDs) == 0 && len(b.EntityIDs) == 0 }
func (b Block) Allows(id string, entities []string) bool {
	return !b.Site && !slices.Contains(b.ContentIDs, id) && !slices.ContainsFunc(entities, func(id string) bool { return slices.Contains(b.EntityIDs, id) })
}
func (b Block) SQL(alias, prefix string) (string, pgx.NamedArgs) {
	if b.Site {
		return "false", pgx.NamedArgs{}
	}
	if b.Empty() {
		return "true", pgx.NamedArgs{}
	}
	return "(NOT (" + alias + ".content_id = ANY(@" + prefix + "_content::text[])) AND NOT (" + alias + ".entity_ids && @" + prefix + "_entities::text[]))", pgx.NamedArgs{prefix + "_content": b.ContentIDs, prefix + "_entities": b.EntityIDs}
}

type locationKey struct{}
type blockKey struct{}
type requestBlock struct {
	tenant, domain string
	block          Block
}

func WithLocation(ctx context.Context, l content.VisitorLocation) context.Context {
	ctx = context.WithValue(ctx, blockKey{}, requestBlock{})
	return context.WithValue(ctx, locationKey{}, l)
}
func Location(ctx context.Context) content.VisitorLocation {
	l, _ := ctx.Value(locationKey{}).(content.VisitorLocation)
	return l
}
func WithBlock(ctx context.Context, tenant, domain string, b Block) context.Context {
	return context.WithValue(ctx, blockKey{}, requestBlock{tenant, domain, b})
}

// Match returns the matched entry kind; unknown locations cannot match geography.
func Match(entry content.LocationEntry, ip netip.Addr, l content.VisitorLocation) string {
	switch {
	case entry.Country != "" && l.Country != "" && entry.Country == l.Country:
		return "country"
	case entry.Subdivision != "" && l.Subdivision != "" && entry.Subdivision == l.Subdivision:
		return "subdivision"
	case entry.Network != "" && ip.IsValid():
		if p, err := netip.ParsePrefix(entry.Network); err == nil && p.Contains(ip.Unmap()) {
			return "network"
		}
	}
	return ""
}

func Resolve(ctx context.Context, q Querier, schema, tenant, domain, ip string) (Block, error) {
	if bound, ok := ctx.Value(blockKey{}).(requestBlock); ok && bound.tenant == tenant && bound.domain == domain {
		return bound.block, nil
	}
	b := Block{ContentIDs: []string{}, EntityIDs: []string{}, Matches: []content.LocationRuleMatch{}}
	addr, _ := netip.ParseAddr(ip)
	rules := pgx.Identifier{schema, "content_location_rules"}.Sanitize()
	entries := pgx.Identifier{schema, "content_location_rule_entries"}.Sanitize()
	rows, err := q.Query(ctx, `SELECT r.id,r.target,r.content_ids,r.entity_ids,coalesce(e.country,''),coalesce(e.subdivision,''),coalesce(e.network::text,'')
 FROM `+rules+` r JOIN `+entries+` e ON e.tenant_id=r.tenant_id AND e.rule_id=r.id
 WHERE r.tenant_id=$1 AND r.enabled AND r.domains @> ARRAY[$2]::text[] ORDER BY r.id,e.position`, tenant, domain)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var m content.LocationRuleMatch
		var e content.LocationEntry
		if err := rows.Scan(&m.RuleID, &m.Target, &m.ContentIDs, &m.EntityIDs, &e.Country, &e.Subdivision, &e.Network); err != nil {
			return b, err
		}
		m.Kind = Match(e, addr, Location(ctx))
		if m.Kind == "" || seen[m.RuleID] {
			continue
		}
		seen[m.RuleID] = true
		b.Matches = append(b.Matches, m)
		switch m.Target {
		case "site":
			b.Site = true
		case "content":
			b.ContentIDs = append(b.ContentIDs, m.ContentIDs...)
		case "entities":
			b.EntityIDs = append(b.EntityIDs, m.EntityIDs...)
		}
	}
	return b, rows.Err()
}
