package signal

import (
	"fmt"
	"regexp"
	"strings"
)

// ActivityIdentity is the authentication state of a measured reader.
type ActivityIdentity string

const (
	ActivityAny       ActivityIdentity = "any"
	ActivityAnonymous ActivityIdentity = "anonymous"
	ActivityAccount   ActivityIdentity = "account"
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

// activityCohortSQL uses only fixed SQL fragments and bound values. It runs
// after aggregation by day and subject, never per page, version or visit.
func activityCohortSQL(cohorts []ActivityCohort) (string, []any) {
	if len(cohorts) == 0 {
		return "''", nil
	}
	parts := make([]string, 0, len(cohorts)*2+1)
	var args []any
	for _, c := range cohorts {
		condition := "1"
		switch c.Identity {
		case ActivityAccount:
			condition = "d.subject_kind='user'"
		case ActivityAnonymous:
			condition = "d.subject_kind<>'user'"
		}
		if len(c.AnyTier) > 0 {
			condition += " AND hasAny(ifNull(p.tiers,[]),?)"
			args = append(args, c.AnyTier)
		}
		parts = append(parts, condition, "?")
		args = append(args, c.ID)
	}
	parts = append(parts, "''")
	return "multiIf(" + strings.Join(parts, ",") + ")", args
}
