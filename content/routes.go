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

// Handler returns the content routes. contentkit.Runtime.Handler serves them at
// the root of its mount; a host mounting them alone puts them under a prefix
// after its own auth middleware populated the identity. Each request runs in
// an access.WithMemo context, so a Gate resolver reads billing at most once
// per request.
func (rt *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	httpapi.Mount(mux, rt, configRoutes)
	httpapi.Mount(mux, rt.posts, postRoutes)
	httpapi.Mount(mux, rt.comments, commentRoutes)
	httpapi.Mount(mux, rt.reactions, reactionRoutes)
	httpapi.Mount(mux, rt.favorites, favoriteRoutes)
	httpapi.Mount(mux, rt.polls, pollRoutes)
	httpapi.Mount(mux, rt, banRoutes)
	httpapi.Mount(mux, rt, moderationRoutes)
	return rt.accessLog(memoized(mux))
}

// Guard serves next only to an actor holding the host permission perm, and
// answers everyone else 403 forbidden, as the content staff routes do.
func (rt *Runtime) Guard(perm string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := rt.requirePerm(r.Context(), rt.actor(r.Context()), perm); err != nil {
			writeErr(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}
