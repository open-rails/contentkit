// Package locationpolicy applies request-local location exclusions. Rules are
// read from Postgres once per request; only the GeoIP file reader is retained.
package locationpolicy

import (
	"context"
	"net/netip"
	"slices"
	"sync"

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

// Block is what the rules matching one visitor exclude. An entity target also
// covers the content its owner entity (a channel) holds.
type Block struct {
	Site       bool
	ContentIDs []string
	EntityIDs  []string
	Matches    []content.LocationRuleMatch
	// Ruled: the domain has enabled rules, so its answers vary by location;
	// SiteRuled: one of them covers the whole site.
	Ruled, SiteRuled bool
}

func (b Block) Empty() bool { return !b.Site && len(b.ContentIDs) == 0 && len(b.EntityIDs) == 0 }

// Allows reports whether content id, in entities and owned by owner ("" for
// none), stays visible.
func (b Block) Allows(id string, entities []string, owner string) bool {
	covered := func(id string) bool { return slices.Contains(b.EntityIDs, id) }
	return !b.Site && !slices.Contains(b.ContentIDs, id) && !slices.ContainsFunc(entities, covered) && (owner == "" || !covered(owner))
}

// SQL is Allows over the content_facets row aliased alias.
func (b Block) SQL(alias, prefix string) (string, pgx.NamedArgs) {
	if b.Site {
		return "false", pgx.NamedArgs{}
	}
	if b.Empty() {
		return "true", pgx.NamedArgs{}
	}
	contents, entities := "@"+prefix+"_content::text[]", "@"+prefix+"_entities::text[]"
	sql := "(NOT (" + alias + ".content_id = ANY(" + contents + ")) AND NOT (" + alias + ".entity_ids && " + entities + ")" +
		" AND (" + alias + ".owner_entity_id IS NULL OR NOT (" + alias + ".owner_entity_id = ANY(" + entities + "))))"
	return sql, pgx.NamedArgs{prefix + "_content": b.ContentIDs, prefix + "_entities": b.EntityIDs}
}

type requestKey struct{}

// request is one request's location state: the visitor's location and the
// rules read for it, shared by every read of the request.
type request struct {
	location content.VisitorLocation
	mu       sync.Mutex
	tenant   string
	domain   string
	block    *Block
	// consulted: a read applied the block, so the answer depends on it.
	consulted bool
}

// WithLocation starts a request's location state for a visitor at l. The
// first Resolve of a domain reads its rules; later ones reuse them.
func WithLocation(ctx context.Context, l content.VisitorLocation) context.Context {
	return context.WithValue(ctx, requestKey{}, &request{location: l})
}

func Location(ctx context.Context) content.VisitorLocation {
	if r, ok := ctx.Value(requestKey{}).(*request); ok {
		return r.location
	}
	return content.VisitorLocation{}
}

// Varies reports whether ctx's answer may depend on the visitor's location:
// the request's domain has a whole-site rule, or a read applied its rules.
func Varies(ctx context.Context) bool {
	r, ok := ctx.Value(requestKey{}).(*request)
	if !ok {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.block != nil && (r.block.SiteRuled || r.consulted && r.block.Ruled)
}

// WithBlock starts a request's location state whose rules for tenant and
// domain were already read as b.
func WithBlock(ctx context.Context, tenant, domain string, b Block) context.Context {
	return context.WithValue(ctx, requestKey{}, &request{location: Location(ctx), tenant: tenant, domain: domain, block: &b})
}

// Prime reads the request's rules without marking them consulted: the
// engine's site check before a route runs.
func Prime(ctx context.Context, q Querier, schema, tenant, domain, ip string) (Block, error) {
	return resolve(ctx, q, schema, tenant, domain, ip, false)
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

// Resolve is the block for a visitor at ip on domain, read once per request.
func Resolve(ctx context.Context, q Querier, schema, tenant, domain, ip string) (Block, error) {
	return resolve(ctx, q, schema, tenant, domain, ip, true)
}

func resolve(ctx context.Context, q Querier, schema, tenant, domain, ip string, consult bool) (Block, error) {
	r, ok := ctx.Value(requestKey{}).(*request)
	if !ok {
		return read(ctx, q, schema, tenant, domain, ip, content.VisitorLocation{})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.block == nil || r.tenant != tenant || r.domain != domain {
		b, err := read(ctx, q, schema, tenant, domain, ip, r.location)
		if err != nil {
			return b, err
		}
		r.tenant, r.domain, r.block, r.consulted = tenant, domain, &b, false
	}
	r.consulted = r.consulted || consult
	return *r.block, nil
}

func read(ctx context.Context, q Querier, schema, tenant, domain, ip string, l content.VisitorLocation) (Block, error) {
	b := Block{ContentIDs: []string{}, EntityIDs: []string{}, Matches: []content.LocationRuleMatch{}}
	addr, _ := netip.ParseAddr(ip)
	rules := pgx.Identifier{schema, "content_location_rules"}.Sanitize()
	entries := pgx.Identifier{schema, "content_location_rule_entries"}.Sanitize()
	domains := pgx.Identifier{schema, "content_location_rule_domains"}.Sanitize()
	contents := pgx.Identifier{schema, "content_location_rule_contents"}.Sanitize()
	entities := pgx.Identifier{schema, "content_location_rule_entities"}.Sanitize()
	// A rule whose last target was deleted remains listed but covers nothing.
	rows, err := q.Query(ctx, `SELECT r.id,r.target,coalesce(c.ids,'{}'::text[]),coalesce(t.ids,'{}'::text[]),coalesce(e.country,''),coalesce(e.subdivision,''),coalesce(e.network::text,'')
 FROM `+rules+` r
 JOIN `+domains+` d ON d.tenant_id=r.tenant_id AND d.rule_id=r.id AND d.domain=$2
 JOIN `+entries+` e ON e.tenant_id=r.tenant_id AND e.rule_id=r.id
 LEFT JOIN LATERAL (SELECT array_agg(c.content_id ORDER BY c.content_id) ids FROM `+contents+` c WHERE c.tenant_id=r.tenant_id AND c.rule_id=r.id) c ON true
 LEFT JOIN LATERAL (SELECT array_agg(t.entity_id ORDER BY t.entity_id) ids FROM `+entities+` t WHERE t.tenant_id=r.tenant_id AND t.rule_id=r.id) t ON true
 WHERE r.tenant_id=$1 AND r.enabled AND (r.target='site' OR c.ids IS NOT NULL OR t.ids IS NOT NULL)
 ORDER BY r.id,e.position`, tenant, domain)
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
		b.Ruled = true
		b.SiteRuled = b.SiteRuled || m.Target == "site"
		m.Kind = Match(e, addr, l)
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
