package signal

// TypeEngagement records measured consumption, never opening a page or reader.
// Its snapshots are cumulative within a bounded segment. A segment's stable
// receipt and revision make retries independent of the number of deliveries.
const TypeEngagement = "engagement"

const (
	MaxEngagementPages = 128
)

const (
	MaxPageRanges = 16
)

const (
	MaxCoverageRanges = 512
)

const (
	MaxWatchedRanges = 256
)

const (
	MaxEngagementMS = 600_000
)

const (
	CoverageScale = 10_000
)

// CoverageRange is an actually visible part of a loaded page, in basis points.
type CoverageRange struct {
	From uint32 `json:"from"`
	To   uint32 `json:"to"`
}

// PageEngagement retains actual exposure and attention separately: reaching a
// page number does not imply that earlier pages were read.
type PageEngagement struct {
	Index    uint32          `json:"index"`
	Ranges   []CoverageRange `json:"ranges"`
	ActiveMS uint32          `json:"active_ms"`
}

// WatchedRange is a played timeline interval. Seeking is not watching.
type WatchedRange struct {
	FromMS uint32 `json:"from_ms"`
	ToMS   uint32 `json:"to_ms"`
}

// Measurement is the trusted, normalized storage form. Covered units are
// hundredths of a reader page, or seconds of a player's timeline. Attention is
// retained per page (per second for players), so short partials can accumulate
// across segments and visits without counting visits themselves.
type Measurement struct {
	VersionID     string   `json:"version_id"`
	VisitID       string   `json:"visit_id"`
	OriginClickID string   `json:"origin_click_id,omitempty"`
	SegmentID     string   `json:"segment_id"`
	ActiveMS      uint32   `json:"active_ms"`
	Total         uint32   `json:"total"`
	Scale         uint32   `json:"scale"`
	Units         []uint32 `json:"units"`
	// Pairs are [logical unit index, active milliseconds], sorted by index.
	Attention [][2]uint64 `json:"attention"`
	// Raw bounded measurements remain available for later quality policies.
	Pages   []PageEngagement `json:"pages,omitempty"`
	Watched []WatchedRange   `json:"watched,omitempty"`
}
