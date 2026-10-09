package access

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Columns names a listing's access columns. Every string is trusted host SQL;
// never build one from user input.
type Columns struct {
	ID     string           // the item's uuid column: "p.id"
	Kind   string           // the item kind, a declared OwnedKinds entry: "post"
	Level  string           // an expression yielding the host's level value: "p.access_policy"
	Levels map[string]Level // host value -> Level; rows of an unmapped value are never admitted
	// Tier is an expression yielding a TierLevel row's tier name; TierName
	// instead names the one declared tier every TierLevel value means.
	Tier     string
	TierName string
	// Scope is the membership scope's uuid column ("p.channel_id") and
	// ScopeKind its declared MemberKinds entry ("channel"); required when a
	// value maps to MembersLevel.
	Scope     string
	ScopeKind string
	ArgPrefix string // named-arg prefix; default "ck_"
}

// Branch classes.
const (
	ClassPublic     = "public"     // public rows
	ClassTier       = "tier"       // rows of a held tier
	ClassMembers    = "members"    // rows of a scope the viewer is a member of
	ClassOwned      = "owned"      // rows the viewer owns
	ClassCandidates = "candidates" // rows an incomplete keyspace may unlock: Gate.Settle decides them
)

// Branch is one access class's predicate.
type Branch struct{ Class, SQL string }

var (
	argPrefixRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,30}$`)
	simpleExpr  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

type sqlPlan struct {
	level, tier, scope, id, arg                    string
	public, tierLevels, membersLevels, paid, tiers []string
	members, owned                                 []string
	tierHeld, membersComplete, ownedComplete       bool
}

// SQL returns one boolean predicate over c admitting the rows the viewer may
// see, plus, while a keyspace is truncated, that keyspace's candidates
// (Gate.Settle decides them). An Unknown filter admits public rows only. The
// text depends only on c, so one prepared statement serves every viewer; args
// are pgx named args (pgx.NamedArgs), the form search.Eligibility takes:
//
//	(p.access_policy = ANY(@ck_open::text[])
//	 OR (p.access_policy = ANY(@ck_members_levels::text[]) AND (p.channel_id = ANY(@ck_members::uuid[]) OR NOT @ck_members_complete::boolean))
//	 OR (p.id = ANY(@ck_owned::uuid[]) AND p.access_policy = ANY(@ck_levels::text[]))
//	 OR (p.access_policy = ANY(@ck_paid_levels::text[]) AND NOT @ck_owned_complete::boolean))
func (f *Filter) SQL(c Columns) (string, map[string]any, error) {
	p, err := f.plan(c)
	if err != nil {
		return "", nil, err
	}
	parts := []string{p.level + " = ANY(@" + p.arg + "open::text[])"}
	if p.tier != "" {
		parts = append(parts, p.tierSQL())
	}
	if len(p.membersLevels) > 0 {
		parts = append(parts, fmt.Sprintf("(%s = ANY(@%smembers_levels::text[]) AND (%s = ANY(@%smembers::uuid[]) OR NOT @%smembers_complete::boolean))",
			p.level, p.arg, p.scope, p.arg, p.arg))
	}
	parts = append(parts, p.ownedSQL(),
		fmt.Sprintf("(%s = ANY(@%spaid_levels::text[]) AND NOT @%sowned_complete::boolean)", p.level, p.arg, p.arg))
	return "(" + strings.Join(parts, "\n OR ") + ")", p.args(), nil
}

// Branches splits SQL by access class for a UNION listing: run each branch
// with the keyset and LIMIT on its own index, then merge (the recipe in
// HOST_INTEGRATION "Listing with access"). Branches that can match nothing for
// this viewer are left out; none means an empty page. All share one args map.
func (f *Filter) Branches(c Columns) ([]Branch, map[string]any, error) {
	p, err := f.plan(c)
	if err != nil {
		return nil, nil, err
	}
	var out []Branch
	if len(p.public) > 0 {
		out = append(out, Branch{ClassPublic, p.level + " = ANY(@" + p.arg + "public_levels::text[])"})
	}
	switch {
	case p.tier != "" && len(p.tiers) > 0:
		out = append(out, Branch{ClassTier, p.tierSQL()})
	case p.tier == "" && p.tierHeld:
		out = append(out, Branch{ClassTier, p.level + " = ANY(@" + p.arg + "tier_levels::text[])"})
	}
	if len(p.members) > 0 {
		out = append(out, Branch{ClassMembers, fmt.Sprintf("(%s = ANY(@%smembers_levels::text[]) AND %s = ANY(@%smembers::uuid[]))",
			p.level, p.arg, p.scope, p.arg)})
	}
	if len(p.owned) > 0 {
		out = append(out, Branch{ClassOwned, p.ownedSQL()})
	}
	var cand []string
	if len(p.membersLevels) > 0 && !p.membersComplete {
		cand = append(cand, p.level+" = ANY(@"+p.arg+"members_levels::text[])")
	}
	if !p.ownedComplete {
		cand = append(cand, p.level+" = ANY(@"+p.arg+"paid_levels::text[])")
	}
	if len(cand) > 0 {
		out = append(out, Branch{ClassCandidates, "(" + strings.Join(cand, " OR ") + ")"})
	}
	return out, p.args(), nil
}

func (p *sqlPlan) tierSQL() string {
	return fmt.Sprintf("(%s = ANY(@%stier_levels::text[]) AND %s = ANY(@%stiers::text[]))", p.level, p.arg, p.tier, p.arg)
}

func (p *sqlPlan) ownedSQL() string {
	return fmt.Sprintf("(%s = ANY(@%sowned::uuid[]) AND %s = ANY(@%slevels::text[]))", p.id, p.arg, p.level, p.arg)
}

func (p *sqlPlan) args() map[string]any {
	open := slices.Clone(p.public)
	if p.tierHeld {
		open = append(open, p.tierLevels...)
	}
	a := map[string]any{
		p.arg + "open":           open,
		p.arg + "public_levels":  p.public,
		p.arg + "tier_levels":    p.tierLevels,
		p.arg + "owned":          p.owned,
		p.arg + "paid_levels":    p.paid,
		p.arg + "levels":         append(slices.Clone(p.public), p.paid...),
		p.arg + "owned_complete": p.ownedComplete,
	}
	if p.tier != "" {
		a[p.arg+"tiers"] = p.tiers
	}
	if len(p.membersLevels) > 0 {
		a[p.arg+"members_levels"] = p.membersLevels
		a[p.arg+"members"] = p.members
		a[p.arg+"members_complete"] = p.membersComplete
	}
	return a
}

func (f *Filter) plan(c Columns) (*sqlPlan, error) {
	g := f.g
	p := &sqlPlan{arg: c.ArgPrefix, level: expr(c.Level), tier: expr(c.Tier), scope: expr(c.Scope), id: expr(c.ID),
		public: []string{}, tierLevels: []string{}, membersLevels: []string{}, paid: []string{}, tiers: []string{}}
	if p.arg == "" {
		p.arg = "ck_"
	}
	switch {
	case !argPrefixRe.MatchString(p.arg):
		return nil, fmt.Errorf("access: Columns.ArgPrefix %q must match %s", p.arg, argPrefixRe)
	case c.ID == "" || c.Level == "" || len(c.Levels) == 0:
		return nil, fmt.Errorf("access: Columns needs ID, Level and Levels")
	case !g.owned[c.Kind]:
		return nil, fmt.Errorf("access: Columns.Kind %q is not in GateConfig.OwnedKinds", c.Kind)
	case c.Tier != "" && c.TierName != "":
		return nil, fmt.Errorf("access: Columns sets both Tier and TierName")
	}
	values := make([]string, 0, len(c.Levels))
	for v := range c.Levels {
		values = append(values, v)
	}
	slices.Sort(values)
	for _, v := range values {
		switch c.Levels[v] {
		case Public:
			p.public = append(p.public, v)
			continue
		case TierLevel:
			p.tierLevels = append(p.tierLevels, v)
		case MembersLevel:
			p.membersLevels = append(p.membersLevels, v)
		case PPV, MembersPPV:
		default:
			return nil, fmt.Errorf("access: Columns.Levels[%q] = %q is not a Level", v, c.Levels[v])
		}
		p.paid = append(p.paid, v)
	}
	if len(p.tierLevels) > 0 {
		switch {
		case c.Tier == "" && c.TierName == "":
			return nil, fmt.Errorf("access: Columns maps a value to TierLevel but sets neither Tier nor TierName")
		case c.TierName != "" && !g.tierSet[c.TierName]:
			return nil, fmt.Errorf("access: Columns.TierName %q is not in GateConfig.Tiers", c.TierName)
		}
	}
	if len(p.membersLevels) > 0 && (c.Scope == "" || !g.members[c.ScopeKind]) {
		return nil, fmt.Errorf("access: Columns maps a value to MembersLevel; it needs Scope and a ScopeKind in GateConfig.MemberKinds (got %q)", c.ScopeKind)
	}
	if c.TierName != "" {
		p.tierHeld = f.Tier(c.TierName)
	}
	if c.Tier != "" {
		for _, t := range g.tiers {
			if f.Tier(t) {
				p.tiers = append(p.tiers, t)
			}
		}
	}
	// An unknown filter admits no candidates: it fails closed.
	p.ownedComplete = f.unknown || f.keyspaceComplete(OwnPrefix(g.tenant, c.Kind))
	p.owned = f.Owned(c.Kind)
	if len(p.membersLevels) > 0 {
		p.membersComplete = f.unknown || f.keyspaceComplete(MembersPrefix(g.tenant, c.ScopeKind))
		p.members = f.Members(c.ScopeKind)
	}
	return p, nil
}

func (f *Filter) keyspaceComplete(prefix string) bool {
	if f.anonymous {
		return true
	}
	_, read := f.held[prefix]
	return read && f.complete[prefix]
}

// expr parenthesizes a host expression unless it is a plain column name.
func expr(s string) string {
	if s == "" || simpleExpr.MatchString(s) {
		return s
	}
	return "(" + s + ")"
}
