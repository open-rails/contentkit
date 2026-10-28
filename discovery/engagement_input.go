package discovery

import (
	"github.com/open-rails/contentkit/signal"
)

type PageEngagement = signal.PageEngagement

type WatchedRange = signal.WatchedRange

// EngagementSnapshot is one cumulative measured segment of a visit. Page
// changes stay local; clients checkpoint about every two minutes and on exit.
// Opening a route, preloading an image, and seeking are not measurements.
type EngagementSnapshot struct {
	ContentID     string           `json:"content_id"`
	VersionID     string           `json:"version_id,omitempty"`
	VisitID       string           `json:"visit_id"`
	OriginClickID string           `json:"origin_click_id,omitempty"`
	SegmentID     string           `json:"segment_id"`
	Revision      uint64           `json:"revision"`
	ActivityToken string           `json:"activity_token"`
	ActiveMS      uint32           `json:"active_ms"`
	Resume        string           `json:"resume,omitempty"`
	Pages         []PageEngagement `json:"pages,omitempty"`
	Watched       []WatchedRange   `json:"watched,omitempty"`
}

// EngagementBatch names the stable browser identity only for a guest. The
// server resolves signed-in subjects and checks every receipt against them.
type EngagementBatch struct {
	Session   string               `json:"session,omitempty"`
	Snapshots []EngagementSnapshot `json:"snapshots"`
}
