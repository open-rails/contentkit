package discovery

// ActiveDays is how many people were active on the site for every UTC day
// of the period, newest first; days without activity are zero.
type ActiveDays struct {
	Days []ActiveDay `json:"days"`
}

// ActiveDay is one day's distinct active people, all sites together, and by
// access: anonymous, account, or the tiers an account's token claimed.
type ActiveDay struct {
	Date     string            `json:"date"`
	People   uint64            `json:"people"`
	ByAccess map[string]uint64 `json:"by_access"`
}

// MyStats is the signed-in reader's account statistics.
type MyStats struct {
	// Completed counts the contents they finished, per kind.
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
