package discovery

import (
	"time"
)

// PopularitySnapshot is the ranking a popular list served.
type PopularitySnapshot struct {
	// Window is the ranking window as requested, canonical: week, 30d, 90d,
	// year, all, month:YYYY-MM, year:YYYY or a range of days.
	Window string `json:"window"`
	// From and Until are its UTC days [From, Until); a window including
	// today counts it so far.
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
	// ComputedAt is when it was ranked; absent while it is not Ready.
	ComputedAt *time.Time `json:"computed_at,omitempty"`
	// Ready is false while the window has not been ranked: the list is in
	// the order of contents without activity, newest first.
	Ready bool `json:"ready"`
	// Stale is a featured ranking whose hourly refresh has since failed.
	Stale bool `json:"stale,omitempty"`
}
