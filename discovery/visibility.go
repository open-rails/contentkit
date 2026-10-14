package discovery

import (
	"context"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/offers"
)

// Visibility is the catalog's one visibility rule (engine spec §4):
// *catalog.Store. A request's Viewer renders it as SQL over content_facets.
type Visibility interface {
	Viewer(ctx context.Context, o catalog.ViewerOptions) (*catalog.Viewer, error)
}

// Covers resolves covers to public renditions, per cover in order:
// versions.Service.
type Covers interface {
	Covers(ctx context.Context, covers []versions.CoverOf) ([][]media.PublicRendition, error)
}

// Sales answers the purchasable offers selling paywalls' keys, by key:
// *offers.Service.
type Sales interface {
	Sales(ctx context.Context, keys []string) (map[string]offers.Sale, error)
}
