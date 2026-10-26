package access

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/open-rails/contentkit/kinds"
)

// Release grants an additional audience access at At. It never removes rights
// granted by the base rule or an earlier release. At is bounded by the content
// and version publication dates. Tier names have no implicit rank.
type Release struct {
	At        time.Time `json:"at"`
	Level     Level     `json:"level"`
	Tiers     []string  `json:"tiers,omitempty"`
	ChannelID string    `json:"group_id,omitempty"`
}

// key compares validated releases independently of input ordering.
func (r Release) key() string {
	r.At = r.At.UTC()
	r.Tiers = slices.Sorted(slices.Values(r.Tiers))
	data, _ := json.Marshal(r)
	return string(data)
}

func (r Rule) validateReleases(cfg kinds.Config) error {
	if len(r.Releases) > 32 {
		return fmt.Errorf("%w: at most 32 releases", ErrRule)
	}
	seen := map[string]bool{}
	for _, release := range r.Releases {
		if release.At.IsZero() {
			return fmt.Errorf("%w: release needs at", ErrRule)
		}
		if release.Level != Public && release.Level != TierLevel && release.Level != ChannelLevel {
			return fmt.Errorf("%w: release audience must be public, tier or group", ErrRule)
		}
		if err := (Rule{Level: release.Level, Tiers: release.Tiers, ChannelID: release.ChannelID}).Validate(cfg); err != nil {
			return err
		}
		key := release.key()
		if seen[key] {
			return fmt.Errorf("%w: repeated release", ErrRule)
		}
		seen[key] = true
	}
	return nil
}

// AudienceTiers is the union of tier audiences released at now. Public access
// is reported separately by Free; entity/PPV rights require entitlement checks.
func (r Rule) AudienceTiers(now time.Time) []string {
	tiers := []string{}
	if r.Level == TierLevel {
		tiers = append(tiers, r.Tiers...)
	}
	for _, release := range r.Releases {
		if !now.Before(release.At) && release.Level == TierLevel {
			tiers = append(tiers, release.Tiers...)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(tiers)))
}

// PublicSQL reports anonymous access from the current rule and mature releases.
// alias is a trusted SQL identifier, validated by Filter.SQL/AudiencesSQL.
func PublicSQL(alias string) string {
	return fmt.Sprintf("(%[1]s.access_level = 'public' OR EXISTS (SELECT 1 FROM jsonb_array_elements(%[1]s.access_releases) release WHERE release->>'level' = 'public' AND (release->>'at')::timestamptz <= now()))", alias)
}

// ReleasedTiersSQL lists the tier audiences that have reached their timestamp.
func ReleasedTiersSQL(alias string) string {
	return fmt.Sprintf("ARRAY(SELECT DISTINCT tier FROM jsonb_array_elements(%[1]s.access_releases) release CROSS JOIN LATERAL jsonb_array_elements_text(release->'tiers') tier WHERE release->>'level' = 'tier' AND (release->>'at')::timestamptz <= now())", alias)
}
