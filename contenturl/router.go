package contenturl

import (
	"log/slog"
	"net/http"
)

// Visibility is the host's verdict on resolved content for one request.
// The zero value is Hidden, so an unset verdict fails closed.
type Visibility int

const (
	// Hidden content answers like an unknown code (the host's 404): no
	// redirect, so nothing about it (its slug) leaks.
	Hidden Visibility = iota
	// Visible content is served and canonicalized.
	Visible
	// Gone content was removed for good: 410, no redirect.
	Gone
	// Preview content is served only to the requesting editor (a draft they
	// may edit): resolved like Visible, never cached or indexed.
	Preview
)

// RouterOptions configures a Router.
type RouterOptions struct {
	// Routes maps content kinds to the host's routes. Required.
	Routes Routes
	// Languages are the site's languages, its default first. Required: every
	// content page is under one (/{lang}/{route}/...), which picks its slug;
	// a request without one redirects to the language Accept-Language names,
	// else the default.
	Languages []string
	// BaseURL ("https://example.com") makes Middleware send the canonical URL
	// as a `Link: <url>; rel="canonical"` header. Empty: no header.
	BaseURL string
	// Visibility is the host's verdict on each resolved link, in order
	// (drafts, removed content, viewer restrictions). nil: every registered
	// code is Visible.
	Visibility func(r *http.Request, links []Link) ([]Visibility, error)
	// Gone answers requests for Gone content. nil: a plain 410.
	Gone http.Handler
	// Logger receives resolution failures (5xx). nil: slog.Default().
	Logger *slog.Logger
}

// Decision is the canonical verdict on a request.
type Decision struct {
	// Matched: the request names visible, routed content. When neither
	// Matched nor Gone, the host serves the request as it would without the
	// router (usually its 404).
	Matched bool
	// Gone: the request names routed content the host removed (410).
	Gone bool
	// Redirect: the path is not canonical; answer 301 to Location.
	Redirect bool
	// Private: matched only for this viewer (Preview); never cache or index it.
	Private  bool
	Link     Link
	Language string
	Path     string // the canonical path
	Location string // Path plus the request's query
}

// Resolved is Handler's response: the link and its canonical path ("" when
// the kind has no route).
type Resolved struct {
	Link
	Path string `json:"path"`
	// VersionID is the live version ?v= names; "" when it names none, and the
	// page shows its language's default edition.
	VersionID string `json:"version_id,omitempty"`
}

// AliasLookup is legacy identifiers to resolve, at most MaxAliases.
type AliasLookup struct {
	Aliases []AliasKey `json:"aliases"`
}

// MaxAliases bounds an AliasLookup.
const MaxAliases = 100

const (
	AliasFound = "found"
)

const (
	AliasNotFound = "not_found" // unknown, or hidden from the request's visitor
)

const (
	AliasGone = "gone" // its content was removed
)

// AliasResult is what one alias resolves to.
type AliasResult struct {
	AliasKey
	// Status is found, not_found or gone.
	Status string `json:"status"`
	// Resolved is a found alias's link and canonical path.
	Resolved *Resolved `json:"resolved,omitempty"`
	// Locator is a found alias's position inside its content, such as a page.
	Locator string `json:"locator,omitempty"`
}

// AliasResults answers an AliasLookup in its order.
type AliasResults struct {
	Aliases []AliasResult `json:"aliases"`
}
