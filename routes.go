package contentkit

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// Route is one HTTP registration on the host's root router: the API's
// subtree. Path ends in "/" and holds every path beneath it; Method is empty
// for every method. The Handler routes within the selected groups itself.
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

// RouteGroups is a set of route entities.
type RouteGroups uint

const (
	// ContentRoutes: the content model (/v1/kinds), content items and their
	// versions, catalog totals, config, comments, reactions, favorites, polls,
	// comment bans and moderation (/v1).
	ContentRoutes RouteGroups = 1 << iota
	// UploadRoutes: media uploads and commits (/v1/media/upload).
	UploadRoutes
	// MediaRoutes: media reads and HLS playlists (/v1/media), and versions'
	// reader plans, reader metadata, forks and downloads
	// (/v1/contents/{id}/versions/{vid}/...).
	MediaRoutes
	// CodeRoutes: content-code lookup (/v1/codes).
	CodeRoutes
	// EntityRoutes: entity listings, pages and writes, and content's
	// memberships (/v1/entities, /v1/contents/{id}/entities).
	EntityRoutes
	// DiscoveryRoutes: lists, search, typeahead, similar and recommended
	// contents, release dates, the reader's content filters and history,
	// signals and exposures (/v1/contents, /v1/search, /v1/me/history, ...).
	DiscoveryRoutes
	// ImportRoutes: the bulk import a legacy importer writes through
	// (/v1/import).
	ImportRoutes

	// AllRoutes is every route group.
	AllRoutes = ContentRoutes | UploadRoutes | MediaRoutes | CodeRoutes | EntityRoutes | DiscoveryRoutes | ImportRoutes
)

var groupModules = []struct {
	group   RouteGroups
	modules []httpapi.Module
	name    string
}{
	{ContentRoutes, []httpapi.Module{httpapi.Kinds, httpapi.Content, httpapi.Contents}, "content"},
	{UploadRoutes, []httpapi.Module{httpapi.Upload}, "upload"},
	{MediaRoutes, []httpapi.Module{httpapi.Media, httpapi.Versions}, "media"},
	{CodeRoutes, []httpapi.Module{httpapi.Codes}, "codes"},
	{EntityRoutes, []httpapi.Module{httpapi.Entities, httpapi.Channels}, "entities"},
	{DiscoveryRoutes, []httpapi.Module{httpapi.Discovery}, "discovery"},
	{ImportRoutes, []httpapi.Module{httpapi.Import}, "import"},
}

// ParseRouteGroups reads route group names ("content", "upload", "media",
// "codes", "entities", "discovery", "import" or "all"), as a server's configuration
// lists them.
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

// Routes returns the selected route groups as one subtree, {Prefix}/v1/, for
// the host's root router (mux.Handle(r.Path, r.Handler) on net/http's
// ServeMux). It fails, before anything mounts, for a group the engine cannot
// serve: one whose module its Config leaves out.
func (e *Engine) Routes(selection Routes) ([]Route, error) {
	if e.erasureRecovery {
		return nil, ErrErasureReplayRequired
	}
	if selection.Groups == 0 || selection.Groups&^AllRoutes != 0 {
		return nil, errors.New("contentkit: Routes.Groups selects no known route group")
	}
	prefix := strings.TrimRight(strings.TrimSpace(selection.Prefix), "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("contentkit: Routes.Prefix %q must start with /", selection.Prefix)
	}
	modules := map[httpapi.Module]http.Handler{}
	for _, g := range groupModules {
		if selection.Groups&g.group == 0 {
			continue
		}
		for _, m := range g.modules {
			if e.modules[m] == nil {
				return nil, fmt.Errorf("contentkit: route group %s needs %s", g.name, needs[g.group])
			}
			modules[m] = e.modules[m]
		}
	}
	return []Route{{Path: prefix + httpapi.Version + "/", Handler: e.dispatch(prefix, modules)}}, nil
}

var needs = map[RouteGroups]string{UploadRoutes: "Config.Media", MediaRoutes: "Config.Media"}

// RouteGroups is every route group the engine can serve: those whose modules
// its Config provides.
func (e *Engine) RouteGroups() RouteGroups {
	var out RouteGroups
	for _, g := range groupModules {
		if !slices.ContainsFunc(g.modules, func(m httpapi.Module) bool { return e.modules[m] == nil }) {
			out |= g.group
		}
	}
	return out
}

// dispatch is the one router under prefix: each request goes to the module
// whose catalog route it matches, its own router seeing the path beneath the
// module's sub-path. Modules sharing a sub-path (kinds, content, groups)
// share it this way.
func (e *Engine) dispatch(prefix string, modules map[httpapi.Module]http.Handler) http.Handler {
	strip := map[httpapi.Module]http.Handler{}
	for m, h := range modules {
		strip[m] = http.StripPrefix(prefix+m.Prefix(), h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.Path, prefix)
		var h http.Handler
		if ok {
			m, _ := httpapi.MatchModule(r.Method, path)
			h = strip[m]
		}
		if h == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}` + "\n"))
			return
		}
		h.ServeHTTP(w, r)
	})
}
