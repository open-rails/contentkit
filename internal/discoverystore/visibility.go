package discoverystore

import (
	"context"

	"github.com/open-rails/contentkit/catalog"
	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
)

// Visibility is the catalog's one visibility rule (engine spec §4):
// *catalog.Store. A request's Viewer renders it as SQL over content_facets.
type Visibility interface {
	Viewer(ctx context.Context, o catalog.ViewerOptions) (*catalogstore.Viewer, error)
}
