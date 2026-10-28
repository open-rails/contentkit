package versions

import (
	"net/http"
)

// PageSpan is a printed page range; a spread is still one reader image.
type PageSpan struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// PageNotice is a quiet note about an automatic page change: removed pages
// (blank, duplicate, or a single covered by a two-page spread) can be
// restored, and an archive reordered into natural name order can be
// dismissed. Neither ever blocks saving or publishing.
type PageNotice struct {
	ID    string       `json:"id"`
	Kind  string       `json:"kind"` // removed | reordered
	Pages []NoticePage `json:"pages"`
}

// NoticePage is one page a notice concerns. A removed page names why
// (blank, duplicate or spread); a reordered page gives its archive position
// and the position applied.
type NoticePage struct {
	ID         string `json:"id"`
	SourceName string `json:"source_name"`
	Reason     string `json:"reason,omitempty"`
	From       int    `json:"from,omitempty"`
	To         int    `json:"to,omitempty"`
	Chapter    int    `json:"chapter,omitempty"` // a removed page still named as this chapter's start
	PreviewURL string `json:"preview_url,omitempty"`
}

// PageNotices are a version's undismissed notices, for its editors.
type PageNotices struct {
	Notices []PageNotice `json:"notices"`
	Cookie  *http.Cookie `json:"-"`
}
