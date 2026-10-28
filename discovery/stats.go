package discovery

// Activity is a kind's measured activity for every UTC day of the period,
// newest first; days without activity are zero.
type Activity struct {
	Days []ActivityDay `json:"days"`
	// Cohorts lists application-defined category IDs in matching priority order.
	Cohorts []string `json:"cohorts"`
}

// ActivityDay is one day's distinct people who read or played the kind, by
// application-defined cohort. Readers counts all, including unmatched people.
type ActivityDay struct {
	Date     string            `json:"date"`
	Readers  uint64            `json:"readers"`
	Cohorts  map[string]uint64 `json:"cohorts"`
	ActiveMS uint64            `json:"active_ms"`
}

// MyStats is the signed-in reader's account statistics.
type MyStats struct {
	// Completed counts the contents they finished, per kind; empty without
	// a signal plane.
	Completed map[string]int64 `json:"completed"`
	// AccessSeconds is elapsed access to each configured tier, from billing's
	// recorded product windows and entitlement history. Overlaps count once;
	// future access does not count. Absent when history is unavailable.
	AccessSeconds map[string]int64 `json:"access_seconds,omitempty"`
	// Comments counts their published comments; CommentLikes and
	// CommentDislikes the reactions those received.
	Comments        int64 `json:"comments"`
	CommentLikes    int64 `json:"comment_likes"`
	CommentDislikes int64 `json:"comment_dislikes"`
}
