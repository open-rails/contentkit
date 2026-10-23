package access

import (
	"fmt"
	"slices"
)

// Anyone is the audience of a free row: an anonymous visitor.
const Anyone = ""

// AudiencesSQL is a text[] expression over the content_facets row aliased
// alias naming who opens it without buying it: Anyone and every one of tiers
// for a free row (public, or past free_after), the tiers among tiers its
// rule names for a tier row, nobody otherwise. Totals count rows per
// audience with it. Args are pgx named args prefixed audience_.
func AudiencesSQL(alias string, tiers []string) (string, map[string]any, error) {
	if !aliasRe.MatchString(alias) {
		return "", nil, fmt.Errorf("access: AudiencesSQL alias %q must match %s", alias, aliasRe)
	}
	for i, t := range tiers {
		if !validTier(t) || slices.Contains(tiers[:i], t) {
			return "", nil, fmt.Errorf("access: tier %q is invalid or repeated", t)
		}
	}
	args := map[string]any{"audience_all": append([]string{Anyone}, tiers...), "audience_tiers": append([]string{}, tiers...)}
	return fmt.Sprintf(`(CASE WHEN %s THEN @audience_all::text[]
 ELSE ARRAY(SELECT DISTINCT t FROM unnest((CASE WHEN %s.access_level='tier' THEN %s.access_tiers ELSE '{}'::text[] END) || %s) t WHERE t=ANY(@audience_tiers::text[]) ORDER BY t) END)`, PublicSQL(alias), alias, alias, ReleasedTiersSQL(alias)), args, nil
}
