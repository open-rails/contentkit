package entitlement

import (
	"fmt"
	"regexp"

	access "github.com/open-rails/contentkit/access"
)

var aliasRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// SQL returns a boolean predicate over the content_facets row aliased alias,
// with its pgx named args (all prefixed access_). Hide admits the rows the
// viewer opens: free, a held tier the rule names, a held key of the rule's
// channel, or an exact held version
// key; plus, while a keyspace is truncated, its candidates, which
// Gate.Settle decides. An Unknown filter admits no candidates. Lock admits
// every row. The text depends only on alias and mode, so one prepared
// statement serves every viewer. Lock mode retains inaccessible items for
// a locked preview; Hide mode excludes them.
func (f *Filter) SQL(alias string, mode access.Mode) (string, map[string]any, error) {
	if !aliasRe.MatchString(alias) {
		return "", nil, fmt.Errorf("access: SQL alias %q must match %s", alias, aliasRe)
	}
	switch mode {
	case access.Lock:
		return "true", map[string]any{}, nil
	case access.Hide:
	default:
		return "", nil, fmt.Errorf("access: unknown listing mode %q", mode)
	}
	tiers := []string{}
	for _, t := range f.g.tiers {
		if f.tiers[t] {
			tiers = append(tiers, t)
		}
	}
	versions := sortedKeys(f.versions)
	parents := make([]string, len(versions))
	for i, version := range versions {
		parents[i] = f.versions[version]
	}
	args := map[string]any{
		"access_tiers":             tiers,
		"access_groups":            f.Channels(),
		"access_versions":          versions,
		"access_version_contents":  parents,
		"access_contents_complete": f.anonymous || f.contentsComplete,
		"access_groups_complete":   f.anonymous || f.entitiesComplete,
	}
	return fmt.Sprintf(`(%[1]s.access_level = 'public'
 OR EXISTS (SELECT 1 FROM jsonb_array_elements(%[1]s.access_releases) release WHERE (release->>'at')::timestamptz <= now()
 AND (release->>'level' = 'public'
 OR (release->>'level' = 'tier' AND ARRAY(SELECT jsonb_array_elements_text(release->'tiers')) && @access_tiers::text[])
 OR (release->>'level' = 'group' AND release->>'group_id' = ANY(@access_groups::text[]))))
 OR (%[1]s.access_level = 'tier' AND %[1]s.access_tiers && @access_tiers::text[])
 OR (%[1]s.access_level = 'group' AND %[1]s.access_entity_id = ANY(@access_groups::text[]))
 OR EXISTS (SELECT 1 FROM unnest(@access_versions::text[], @access_version_contents::text[]) held(version_id, content_id)
  WHERE held.version_id = %[1]s.version_id AND held.content_id = %[1]s.content_id)
 OR NOT @access_contents_complete::boolean
 OR (NOT @access_groups_complete::boolean AND (%[1]s.access_level = 'group' OR EXISTS (SELECT 1 FROM jsonb_array_elements(%[1]s.access_releases) release WHERE release->>'level' = 'group' AND (release->>'at')::timestamptz <= now()))))`, alias), args, nil
}
