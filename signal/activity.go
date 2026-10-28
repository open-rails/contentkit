package signal

import (
	"fmt"
	"regexp"
	"strings"
)

// ActivityIdentity is the authentication state of a measured reader.
type ActivityIdentity string

const (
	ActivityAny ActivityIdentity = "any"
)

const (
	ActivityAnonymous ActivityIdentity = "anonymous"
)

const (
	ActivityAccount ActivityIdentity = "account"
)

// ActivityCohort is an application-defined daily readership bucket. The first
// matching cohort wins, so put a tier-specific cohort before its account
// fallback. AnyTier matches trusted eligibility observed at any time that day;
// a client cannot choose its cohort. No cohorts means totals only.
type ActivityCohort struct {
	ID       string           `json:"id" yaml:"id"`
	Identity ActivityIdentity `json:"identity" yaml:"identity"`
	AnyTier  []string         `json:"any_tier,omitempty" yaml:"any_tier,omitempty"`
}

var activityCohortID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidateActivityCohorts rejects ambiguous IDs and impossible conditions.
func ValidateActivityCohorts(cohorts []ActivityCohort) error {
	if len(cohorts) > 16 {
		return fmt.Errorf("signal: at most 16 activity cohorts are supported")
	}
	seen := map[string]bool{}
	for _, c := range cohorts {
		if !activityCohortID.MatchString(c.ID) || seen[c.ID] {
			return fmt.Errorf("signal: invalid or duplicate activity cohort ID %q", c.ID)
		}
		seen[c.ID] = true
		switch c.Identity {
		case ActivityAny, ActivityAnonymous, ActivityAccount:
		default:
			return fmt.Errorf("signal: activity cohort %q has invalid identity %q", c.ID, c.Identity)
		}
		if c.Identity == ActivityAnonymous && len(c.AnyTier) > 0 {
			return fmt.Errorf("signal: anonymous activity cohort %q cannot require account tiers", c.ID)
		}
		tiers := map[string]bool{}
		for _, tier := range c.AnyTier {
			if strings.TrimSpace(tier) == "" || tiers[tier] {
				return fmt.Errorf("signal: activity cohort %q has an empty or duplicate tier", c.ID)
			}
			tiers[tier] = true
		}
	}
	return nil
}
