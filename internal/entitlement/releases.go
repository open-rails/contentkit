package entitlement

import (
	"fmt"
)

// PublicSQL reports anonymous access from the current rule and mature releases.
// alias is a trusted SQL identifier, validated by Filter.SQL/AudiencesSQL.
func PublicSQL(alias string) string {
	return fmt.Sprintf("(%[1]s.access_level = 'public' OR EXISTS (SELECT 1 FROM jsonb_array_elements(%[1]s.access_releases) release WHERE release->>'level' = 'public' AND (release->>'at')::timestamptz <= now()))", alias)
}

// ReleasedTiersSQL lists the tier audiences that have reached their timestamp.
func ReleasedTiersSQL(alias string) string {
	return fmt.Sprintf("ARRAY(SELECT DISTINCT tier FROM jsonb_array_elements(%[1]s.access_releases) release CROSS JOIN LATERAL jsonb_array_elements_text(release->'tiers') tier WHERE release->>'level' = 'tier' AND (release->>'at')::timestamptz <= now())", alias)
}
