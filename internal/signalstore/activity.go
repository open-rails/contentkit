package signalstore

import (
	"strings"

	signal "github.com/open-rails/contentkit/signal"
)

// activityCohortSQL uses only fixed SQL fragments and bound values. It runs
// after aggregation by day and subject, never per page, version or visit.
func activityCohortSQL(cohorts []signal.ActivityCohort) (string, []any) {
	if len(cohorts) == 0 {
		return "''", nil
	}
	parts := make([]string, 0, len(cohorts)*2+1)
	var args []any
	for _, c := range cohorts {
		condition := "1"
		switch c.Identity {
		case signal.ActivityAccount:
			condition = "d.subject_kind='user'"
		case signal.ActivityAnonymous:
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
