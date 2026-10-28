package contentstore

import (
	"net/http"

	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
)

var configRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/config", Resource: "config", Auth: httpapi.Open,
		Doc:       "What the content module allows: which interactions signed-out visitors may make, and the longest comment.",
		Responses: []httpapi.Reply{httpapi.OK(content.Config{})}},
		Serve: httpapi.H((*Runtime).handleConfig)},
}

func (rt *Runtime) config() content.Config {
	return content.Config{Anonymous: rt.anonymous, CommentMaxLength: rt.commentMax}
}

func (rt *Runtime) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, rt.config())
}

// participant admits a signed-in person, and an anonymous visitor when
// allowed (Options.Anonymous): 401 for anyone else anonymous, 403 for an
// application. Interactions are a person's.
func participant(actor access.Actor, allowed bool) error {
	switch {
	case actor.Member():
		return nil
	case actor.Anonymous || actor.ID == "":
		if allowed {
			return nil
		}
		return errUnauthorized
	}
	return errForbidden
}
