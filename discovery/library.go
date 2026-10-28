package discovery

// LibraryQuery pages a viewer's library.
type LibraryQuery struct {
	// Language picks the titles; the configured languages follow.
	Language string
	Cursor   string // the previous page's next_cursor
	Limit    int    // 20 by default, at most 100
}

// LibraryItem groups the independently purchased editions of one gallery.
type LibraryItem struct {
	ContentID string `json:"content_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Language  string `json:"language,omitempty"`
	// Deleted marks a purchase whose content is deleted: it stays in the
	// buyer's library but is unreadable unless restored.
	Deleted bool `json:"deleted"`
	// Whole is always false: a gallery parent does not grant audience access.
	// VersionIDs lists only currently eligible, individually granted editions.
	Whole      bool     `json:"whole"`
	VersionIDs []string `json:"version_ids"`
}

// LibraryPage is one page of a library, newest content first.
type LibraryPage struct {
	Items      []LibraryItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
	// Complete is false when the viewer holds more keys than one billing read
	// returns (access.HeldLimit); the library lists those it read.
	Complete bool `json:"complete"`
}
