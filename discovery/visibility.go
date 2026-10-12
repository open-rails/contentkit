package discovery

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// Visibility is the catalog's one visibility rule (engine spec §4) for the
// reader of one request.
type Visibility interface {
	Viewer(ctx context.Context, o ViewerOptions) (Viewer, error)
}

// ViewerOptions describe one request's reader.
type ViewerOptions struct {
	Actor access.Actor
	// Host is the request's host; it picks the site scope.
	Host string
	// Language, when set, lists only content with a live version serving it.
	Language string
	// Mode is lock (list, mark locked) or hide (only what the reader opens).
	Mode access.Mode
}

// Viewer is one request's reader under the rule: its site, entitlements and
// content filters resolved once.
type Viewer interface {
	// Listed is a boolean SQL predicate over the content_facets row aliased
	// alias: the versions the reader's listings show, with its pgx named
	// args (prefixed cv_). In hide mode it may admit versions a truncated
	// keyspace leaves undecided; their cards' decisions settle them.
	Listed(alias string) (string, pgx.NamedArgs)
}

// Covers resolves covers to public renditions, per cover in order:
// versions.Service.
type Covers interface {
	Covers(ctx context.Context, covers []versions.CoverOf) ([][]media.PublicRendition, error)
}
