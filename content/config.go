package content

import (
	"net/http"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Anonymous is what signed-out visitors may do. The zero value allows none of
// it: an anonymous attempt answers 401 unauthorized.
type Anonymous struct {
	// Comments: comment and reply under a name (anon_name).
	Comments bool `json:"comments"`
	// Reactions: like and dislike works, posts and comments, keyed by IP.
	Reactions bool `json:"reactions"`
	// Votes: vote in multiple-choice polls, keyed by IP. Free-text answers
	// always need a signed-in actor.
	Votes bool `json:"votes"`
}

// Config is what the content module allows, so clients choose between a
// sign-in prompt and an anonymous form, and bound a comment, before acting.
type Config struct {
	Anonymous Anonymous `json:"anonymous"`
	// CommentMaxLength is the longest comment, in characters.
	CommentMaxLength int `json:"comment_max_length"`
}

// DefaultCommentMaxLength is Options.CommentMaxLength unset.
const DefaultCommentMaxLength = 400

var configRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/config", Resource: "config", Auth: httpapi.Public,
		Doc:       "What the content module allows: which interactions signed-out visitors may make, and the longest comment.",
		Responses: []httpapi.Reply{httpapi.OK(Config{})}},
		Serve: httpapi.H((*Runtime).handleConfig)},
}

func (rt *Runtime) config() Config {
	return Config{Anonymous: rt.anonymous, CommentMaxLength: rt.commentMax}
}

func (rt *Runtime) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, rt.config())
}

// participant refuses an anonymous actor unless allowed: interactions open to
// anonymous visitors only as Options.Anonymous says.
func participant(actor access.Actor, allowed bool) error {
	if allowed || (!actor.Anonymous && actor.ID != "") {
		return nil
	}
	return errUnauthorized
}
