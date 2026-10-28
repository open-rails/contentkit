package discovery

import (
	"time"
)

// SitemapPage is one page of a site's public pages of one kind, by id.
type SitemapPage struct {
	Entries []SitemapEntry `json:"entries"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// SitemapEntry is one public page: a content item or entity.
type SitemapEntry struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	// Path is the page in the first of the site's languages it serves;
	// Languages are its page in each, for hreflang alternates.
	Path      string         `json:"path"`
	LastMod   time.Time      `json:"lastmod"`
	Languages []SitemapAlias `json:"languages"`
}

// SitemapAlias is a page in one language. A content page's Image is the
// cover of the edition the language opens by default, its widest rendition,
// where its kind's locked view shows covers.
type SitemapAlias struct {
	Language string `json:"language"`
	Path     string `json:"path"`
	Image    string `json:"image,omitempty"`
}

// SitemapOptions select one page of the feed.
type SitemapOptions struct {
	// Host picks the site scope.
	Host string
	Kind string
	// Cursor is the previous page's NextCursor.
	Cursor string
	// Limit defaults to 1000 and is capped at 10000.
	Limit int
}
