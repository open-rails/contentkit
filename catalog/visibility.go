package catalog

import (
	"github.com/open-rails/contentkit/access"
)

// ViewerOptions describes one request's reader.
type ViewerOptions struct {
	Actor access.Actor
	// Host is the request's host; it picks the site (kinds.Config.DomainFor).
	Host string
	// Language, when set, lists only content with a live version serving it.
	Language string
	// Mode defaults to access.Lock.
	Mode access.Mode
	// EditorEntities are owner entities (channels) whose editors see their whole
	// content in lists; the viewer's editorship of each is checked.
	EditorEntities []string
	// Requested are entities a list requires (an entity's page): the viewer's
	// content filters do not hide them, so the page shows what the entity's
	// count counts.
	Requested []string
}
