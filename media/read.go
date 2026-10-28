package media

import (
	"time"

	"github.com/open-rails/contentkit/media/token"
)

// DeliveryMode is how viewers with access present their item token.
type DeliveryMode string

const (
	// DeliverCookie (default) sets the item cookie; URLs are plain.
	DeliverCookie DeliveryMode = "cookie"
)

const (
	// DeliverURL appends ?t= to every URL: apps and clients without cookies.
	DeliverURL DeliveryMode = "url"
)

// CookieName is the media gateway's cookie.
const CookieName = token.CookieName

// Delivery is the host's signing configuration; URLs are at the
// registry's BaseURL.
type Delivery struct {
	Mode DeliveryMode
	// CookieDomain is the site's registrable domain, e.g. "doujins.ai";
	// required in cookie mode, because a host-only cookie never reaches media.
	CookieDomain string
	// SigningKey is the current key of the ring the media gateway verifies.
	SigningKey token.Key
	// TTL is the minimum token lifetime (default 1h); expiries round up to
	// Window (default token.DefaultWindow, whole seconds).
	TTL, Window time.Duration
}

// ReadOptions select what a read returns.
type ReadOptions struct {
	Prefix        string // only files under this path prefix ("low-res/")
	Offset, Limit int    // the range that gets URLs
	// Paths selects at most 64 exact file paths to receive URLs, independent
	// of ordinal position. All allowed metadata remains in manifest order.
	// It cannot be combined with a nonzero Offset or Limit.
	Paths    []string
	Download bool // URLs serve each file as a download under its name
	// Editor adds an editor's uploads with their edit, frame, meta,
	// pending, failure, editor view and retained source URL, unattached ones
	// included.
	Editor bool
	// Status returns only whole-manifest processing counts, for editors.
	// It issues no URLs or editor-view work and ignores pagination/prefix.
	Status bool
}
