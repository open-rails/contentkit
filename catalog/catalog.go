// Package catalog is ContentKit's own content: content items (a gallery, a
// movie, a creator or blog post), their per-language text, creators, versions
// with their text and segments, the state machine (draft, published,
// scheduled, deleted), the listing projection (content_facets), the one
// visibility rule every read applies, the retained event log and the catalog
// totals. Kinds are configuration (package kinds); nothing here names a host's
// nouns.
package catalog

import (
	"context"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
)

// DefaultDeletedRetention is how long deleted content keeps its
// media and can be restored before PurgeDeleted erases it.
const DefaultDeletedRetention = 30 * 24 * time.Hour

// Standing reads a signed-in person's standing with content items and
// versions, per ref in order: *discovery.Service.
type Standing interface {
	Standing(ctx context.Context, actor access.Actor, refs []contentref.ContentRef) ([]ViewerState, error)
}

// Sanitizer cleans rich text on write: content.ContentProcessor.
type Sanitizer = content.ContentProcessor
