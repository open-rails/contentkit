package access

import (
	"fmt"
	"regexp"
	"slices"
)

// Columns maps host SQL to access levels for taxonomy.Totals, the #116
// catalog-totals stopgap that ContentKit #122 replaces with counts over
// content_facets. Every string is trusted host SQL.
type Columns struct {
	Level  string           // an expression yielding the host's level value
	Levels map[string]Level // host value -> Level
	// Tier is an expression yielding a TierLevel row's tier name; TierName
	// instead names the one tier every TierLevel value means.
	Tier      string
	TierName  string
	ArgPrefix string // named-arg prefix; default "ck_"
}

// Anyone is the audience of every public row: an anonymous visitor.
const Anyone = ""

var (
	argPrefixRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,30}$`)
	simpleExpr  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

// Audiences returns a text[] expression over c naming who opens a row without
// buying it: Anyone and every one of tiers for a public row, the row's tier
// for a TierLevel row of one of tiers, nobody otherwise. Args are pgx named
// args under c's prefix.
func (c Columns) Audiences(tiers []string) (string, map[string]any, error) {
	p := c.ArgPrefix
	if p == "" {
		p = "ck_"
	}
	switch {
	case !argPrefixRe.MatchString(p):
		return "", nil, fmt.Errorf("access: Columns.ArgPrefix %q must match %s", p, argPrefixRe)
	case c.Level == "" || len(c.Levels) == 0:
		return "", nil, fmt.Errorf("access: Columns needs Level and Levels")
	case c.Tier != "" && c.TierName != "":
		return "", nil, fmt.Errorf("access: Columns sets both Tier and TierName")
	}
	seen := map[string]bool{}
	for _, t := range tiers {
		if !validTier(t) || seen[t] {
			return "", nil, fmt.Errorf("access: tier %q is invalid or repeated", t)
		}
		seen[t] = true
	}
	if c.TierName != "" && !seen[c.TierName] {
		return "", nil, fmt.Errorf("access: Columns.TierName %q is not among the tiers", c.TierName)
	}
	public, tierLevels := []string{}, []string{}
	values := make([]string, 0, len(c.Levels))
	for v := range c.Levels {
		values = append(values, v)
	}
	slices.Sort(values)
	for _, v := range values {
		switch c.Levels[v] {
		case Public:
			public = append(public, v)
		case TierLevel:
			tierLevels = append(tierLevels, v)
		case GroupLevel, PPV, GroupPPV:
		default:
			return "", nil, fmt.Errorf("access: Columns.Levels[%q] = %q is not a Level", v, c.Levels[v])
		}
	}
	if len(tierLevels) > 0 && c.Tier == "" && c.TierName == "" {
		return "", nil, fmt.Errorf("access: Columns maps a value to TierLevel but sets neither Tier nor TierName")
	}
	args := map[string]any{p + "public_levels": public, p + "tier_levels": tierLevels,
		p + "audiences": append([]string{Anyone}, tiers...), p + "tiers": append([]string{}, tiers...)}
	tier := "NULL::text"
	switch {
	case c.Tier != "":
		tier = expr(c.Tier)
	case c.TierName != "":
		tier, args[p+"tier_name"] = "@"+p+"tier_name::text", c.TierName
	}
	level := expr(c.Level)
	return fmt.Sprintf(`(CASE WHEN %[1]s = ANY(@%[2]spublic_levels::text[]) THEN @%[2]saudiences::text[]
 WHEN %[1]s = ANY(@%[2]stier_levels::text[]) AND %[3]s = ANY(@%[2]stiers::text[]) THEN ARRAY[%[3]s]::text[]
 ELSE '{}'::text[] END)`, level, p, tier), args, nil
}

// expr parenthesizes a host expression unless it is a plain column name.
func expr(s string) string {
	if s == "" || simpleExpr.MatchString(s) {
		return s
	}
	return "(" + s + ")"
}
