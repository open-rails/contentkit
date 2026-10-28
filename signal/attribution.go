package signal

import (
	"time"
)

// TypeClick is the selection signal type Attribution joins to exposures.
const TypeClick = "click"

const (
	DefaultAttributionRenders = 500
)

const (
	DefaultAttributionClicks = 5000
)

// AttributionOptions selects an evaluation export. Stage is required. A paged
// export passes the previous page's Next as After with the same Window, Stage
// and Surface; Limit and ClickLimit may change between pages.
type AttributionOptions struct {
	// Window bounds exposure and click days (zero = all time).
	Window Window
	// Stage is the exposure stage clicks are attributed against (required).
	Stage ExposureStage
	// Surface restricts renders to one surface (empty = all).
	Surface string
	// After is the opaque cursor from the previous page's Next (empty = start).
	After string
	// Limit caps renders per page (default DefaultAttributionRenders).
	Limit int
	// ClickLimit caps click rows per page, attributed and unattributed
	// together (default DefaultAttributionClicks). A render with more clicks
	// than fit continues on the next page.
	ClickLimit int
}

// AttributedClick is a canonical click carrying a render id.
type AttributedClick struct {
	ContentRef
	Subject    Subject // who clicked; may differ from the render's subject
	Position   uint32  // position claimed by the click
	OccurredAt time.Time
	EventID    string
	// Exposed reports whether the clicked content is in the render's list at
	// the requested stage. False means "not exposed at this stage", never negative.
	Exposed bool
	// Visits contain only measurements carrying this exact click ID and subject.
	// An empty list means no engagement was observed, not a proven bounce.
	Visits []AttributedVisit
}

type AttributedVisit struct {
	VisitID    string
	VersionIDs []string
	ActiveMS   uint64
	Coverage   float64
	Completed  bool
}

// AttributedRender is one render at one stage with its clicks joined.
type AttributedRender struct {
	RenderID    string
	QueryID     string
	SearchQuery string
	Surface     string
	Ranker      string
	Language    string
	Subject     Subject
	OccurredAt  time.Time
	Shown       []Placement
	Clicks      []AttributedClick
	// Continued is true when earlier clicks of this render were on the previous
	// page: the header repeats, the clicks do not. Merge by RenderID.
	Continued bool
}

// AttributionPage is one page of the evaluation export.
type AttributionPage struct {
	Renders []AttributedRender
	// Unattributed are clicks in the window whose render has no exposure at
	// the requested stage (prefetched, cancelled, hidden or never recorded):
	// they cannot label anything.
	Unattributed []AttributedClick
	// Next is the cursor for the following page; empty when done.
	Next string
}
