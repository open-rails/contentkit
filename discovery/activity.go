package discovery

// Activity types a browser reports. Everything else (reactions, favorites,
// hides and searches) the server logs where it happens.
const (
	ActivityOpen     = "open"
	ActivityProgress = "progress"
)

// MaxActivityEvents bounds one activity report.
const MaxActivityEvents = 50

// Activity is what a browser reports for its reader: the signed-in account,
// else the anonymous id in the ContentKit-Anonymous-Id header. The server
// stamps the site, access, local UTC offset and time.
type Activity struct {
	// Language is the content language read; the request's by default.
	Language string          `json:"language,omitempty"`
	Events   []ActivityEvent `json:"events"`
}

// ActivityEvent is an open of a content item or a report of reading it.
type ActivityEvent struct {
	// Type is open or progress.
	Type      string `json:"type"`
	ContentID string `json:"content_id"`
	// RequestID, Surface and Position name the list an open came from: the
	// request_id of the search or list response, its surface and the
	// 1-based position; empty for an open from outside.
	RequestID string `json:"request_id,omitempty"`
	Surface   string `json:"surface,omitempty"`
	Position  uint32 `json:"position,omitempty"`
	// ActiveMS is the time the reader was active on a visible tab since the
	// previous report, at most five minutes and a little.
	ActiveMS uint32 `json:"active_ms,omitempty"`
	// Furthest and Current are 1-based pages of a reader or seconds of a
	// player; Length is the pages or seconds there are.
	Furthest uint32 `json:"furthest,omitempty"`
	Current  uint32 `json:"current,omitempty"`
	Length   uint32 `json:"length,omitempty"`
}

// AnonymousLink is sent once at sign-in: the anonymous id this browser used
// signed in as the request's account, so its reading becomes the account's.
type AnonymousLink struct {
	AnonymousID string `json:"anonymous_id"`
}
