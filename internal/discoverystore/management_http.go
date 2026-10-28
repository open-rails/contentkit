package discoverystore

import (
	"net/http"

	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

var managementRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage/search-suggestions", Resource: "discovery", Auth: httpapi.Public,
		Doc: "Private quick navigation to authorized content/entity editors. Requires a signed-in person or application. Current own titles, names and aliases match literal substrings with prefix bias; includes drafts, held edits and internal entities within management authority. No associated metadata/body matching or public-search fallback. Sections require explicit include flags; forbidden sections are omitted when another section is authorized. Returns lean name targets, with no content body or cover reads.",
		Query: []httpapi.Param{httpapi.Text("q", "the partial name"), httpapi.List("content_kinds", "configured content kinds; all only when include_contents is explicitly true"),
			httpapi.List("entity_kinds", "configured entity kinds; all only when include_entities is explicitly true"), httpapi.Bool("include_contents", "explicitly request authorized content targets"),
			httpapi.Bool("include_entities", "explicitly request entity targets; requires entity-management authority"), httpapi.Text("display_language", "configured name language; normal text fallback applies"), httpapi.Int("limit", "per section: 8 by default, from 1 through 20")},
		Responses: []httpapi.Reply{httpapi.OK(discovery.Typeahead{})}, Errors: []string{httpapi.CodeUnauthorized, httpapi.CodeForbidden, httpapi.CodeUnavailable}}, Serve: httpapi.H(handler.managementSuggestions)},
}

func (h handler) managementSuggestions(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	contents, err := optionalFlag(r, "include_contents")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	entities, err := optionalFlag(r, "include_entities")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	out, err := h.s.ManagementSuggestions(r.Context(), discovery.ManagementSuggestionsOptions{Actor: gate.Actor(r.Context()), Query: q.Get("q"),
		DisplayLanguage: q.Get("display_language"), ContentKinds: list(q.Get("content_kinds")), EntityKinds: list(q.Get("entity_kinds")),
		IncludeContents: contents, IncludeEntities: entities, Limit: limit})
	h.reply(w, r, out, err)
}
