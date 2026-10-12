package contentkit

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// Route is one HTTP registration on the host's root router: a route group's
// subtree. Path ends in "/" and holds every path beneath it; Method is empty
// for every method. The Handler routes within the group itself.
type Route struct {
	Method  string
	Path    string
	Handler http.Handler
}

// Routes selects the HTTP surface: the route groups under Prefix.
type Routes struct {
	// Prefix is where the API is mounted: "/api/contentkit" serves
	// /api/contentkit/v1/*. Empty is the root.
	Prefix string
	// Groups are the route groups served. Required.
	Groups RouteGroups
}

// RouteGroups is a set of route groups.
type RouteGroups uint

const (
	// ContentRoutes: config, posts, comments, reactions, favorites, polls,
	// comment bans and moderation (/v1).
	ContentRoutes RouteGroups = 1 << iota
	// UploadRoutes: media uploads and commits (/v1/media/upload).
	UploadRoutes
	// MediaRoutes: media reads and HLS playlists (/v1/media).
	MediaRoutes
	// CodeRoutes: content-code lookup (/v1/codes).
	CodeRoutes
	// TaxonomyRoutes: the taxonomy admin API (/v1/taxonomy).
	TaxonomyRoutes
	// CatalogRoutes: the public catalog totals (/v1/catalog).
	CatalogRoutes
	// NotificationRoutes: the user inbox and the host's publish route
	// (/v1/notifications).
	NotificationRoutes

	// AllRoutes is every route group.
	AllRoutes = ContentRoutes | UploadRoutes | MediaRoutes | CodeRoutes | TaxonomyRoutes | CatalogRoutes | NotificationRoutes
)

var groupModules = []struct {
	group  RouteGroups
	module httpapi.Module
	name   string
}{
	{ContentRoutes, httpapi.Content, "content"},
	{UploadRoutes, httpapi.Upload, "upload"},
	{MediaRoutes, httpapi.Media, "media"},
	{CodeRoutes, httpapi.Codes, "codes"},
	{TaxonomyRoutes, httpapi.Taxonomy, "taxonomy"},
	{CatalogRoutes, httpapi.PublicCatalog, "catalog"},
	{NotificationRoutes, httpapi.Notifications, "notifications"},
}

// ParseRouteGroups reads route group names ("content", "upload", "media",
// "codes", "taxonomy", "catalog", "notifications" or "all"), as a server's
// configuration lists them.
func ParseRouteGroups(names []string) (RouteGroups, error) {
	var out RouteGroups
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "all" {
			out |= AllRoutes
			continue
		}
		found := false
		for _, g := range groupModules {
			if g.name == n {
				out, found = out|g.group, true
			}
		}
		if !found {
			return 0, fmt.Errorf("contentkit: unknown route group %q", n)
		}
	}
	return out, nil
}

// Routes returns the selected route groups, each a subtree under
// Prefix+"/v1" for the host's root router (mux.Handle(r.Path, r.Handler) on
// net/http's ServeMux; a nested group's longer path wins). It fails, before
// anything mounts, for a group the engine cannot serve: one whose module its
// Config or Deps leave out.
func (e *Engine) Routes(selection Routes) ([]Route, error) {
	if selection.Groups == 0 || selection.Groups&^AllRoutes != 0 {
		return nil, errors.New("contentkit: Routes.Groups selects no known route group")
	}
	prefix := strings.TrimRight(strings.TrimSpace(selection.Prefix), "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("contentkit: Routes.Prefix %q must start with /", selection.Prefix)
	}
	var out []Route
	for _, g := range groupModules {
		if selection.Groups&g.group == 0 {
			continue
		}
		h := e.modules[g.module]
		if h == nil {
			return nil, fmt.Errorf("contentkit: route group %s needs %s", g.name, needs(g.module))
		}
		mount := prefix + g.module.Prefix()
		out = append(out, Route{Path: mount + "/", Handler: http.StripPrefix(mount, h)})
	}
	return out, nil
}

func needs(m httpapi.Module) string {
	switch m {
	case httpapi.Upload, httpapi.Media:
		return "Config.Media"
	case httpapi.Codes:
		return "a route on a content or group kind in Config.Content"
	case httpapi.Taxonomy:
		return "group kinds in Config.Content"
	case httpapi.PublicCatalog:
		return "Deps.Listing and group kinds in Config.Content"
	}
	return "its module"
}
