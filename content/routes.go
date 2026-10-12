package content

import (
	"net/http"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// The content module's routes are declared in each resource's file; this
// registers them in the catalog in the order they are documented.
func init() {
	httpapi.Register(httpapi.Content, configRoutes)
	httpapi.Register(httpapi.Content, postRoutes)
	httpapi.Register(httpapi.Content, commentRoutes)
	httpapi.Register(httpapi.Content, reactionRoutes)
	httpapi.Register(httpapi.Content, favoriteRoutes)
	httpapi.Register(httpapi.Content, pollRoutes)
	httpapi.Register(httpapi.Content, banRoutes)
	httpapi.Register(httpapi.Content, moderationRoutes)
}

var sortParam = httpapi.Text("sort", "likes (most liked), best (Wilson lower bound) or newest (the default)")

// Handler returns the content routes, each behind its tier's gate over
// Options.Auth: the engine's content route group (/v1). Each request
// runs in an access.WithMemo context, so a Gate resolver reads billing at
// most once per request.
func (rt *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	g := rt.guard.Guard
	httpapi.Mount(mux, g, rt, configRoutes)
	httpapi.Mount(mux, g, rt.posts, postRoutes)
	httpapi.Mount(mux, g, rt.comments, commentRoutes)
	httpapi.Mount(mux, g, rt.reactions, reactionRoutes)
	httpapi.Mount(mux, g, rt.favorites, favoriteRoutes)
	httpapi.Mount(mux, g, rt.polls, pollRoutes)
	httpapi.Mount(mux, g, rt, banRoutes)
	httpapi.Mount(mux, g, rt, moderationRoutes)
	return rt.accessLog(memoized(mux))
}
