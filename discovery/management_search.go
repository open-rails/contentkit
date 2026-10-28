package discovery

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
)

// ManagementCatalog keeps editor authorization with the catalog. The standard
// *catalog.Store implements it; a custom Visibility may forward this capability.
type ManagementCatalog interface {
	ManagementSearch(context.Context, access.Actor, catalog.ManagementSearchQuery) (catalog.ManagementNames, error)
}

type ManagementSuggestionsOptions struct {
	Actor                            access.Actor
	Query, DisplayLanguage           string
	ContentKinds, EntityKinds        []string
	IncludeContents, IncludeEntities bool
	Limit                            int
}
