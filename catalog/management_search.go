package catalog

import (
	"github.com/open-rails/contentkit/entities"
)

// ManagementName is the current name of an item its caller may edit. No body,
// media, public-listing policy or audience billing check is needed to navigate.
type ManagementName struct{ ID, Kind, Title, Language string }

type ManagementNames struct {
	Contents []ManagementName
	Entities []entities.EntityCard
}

type ManagementSearchQuery struct {
	Query, Language                  string
	ContentKinds, EntityKinds        []string
	IncludeContents, IncludeEntities bool
	Limit                            int
}
