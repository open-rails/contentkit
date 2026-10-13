// Package kindsroute serves the site's public content model: the part of
// kinds.Config clients render kinds, groups and content filters with.
// Restrictions, sites and token claims stay on the server.
package kindsroute

import (
	"encoding/json"
	"net/http"

	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/kinds"
)

// ContentModel is the public part of kinds.Config.
type ContentModel struct {
	Languages      []string                   `json:"languages"`
	Tiers          []string                   `json:"tiers"`
	Kinds          map[string]kinds.Kind      `json:"kinds"`
	GroupKinds     map[string]kinds.GroupKind `json:"group_kinds"`
	ContentFilters []kinds.ContentFilter      `json:"content_filters"`
	// Signals: the site records signals and exposures. Without, clients send
	// none, and history, similar and recommendations answer not_configured.
	Signals bool `json:"signals"`
}

// Public is c's public part, with empty lists and maps rather than null.
func Public(c kinds.Config, signals bool) ContentModel {
	m := ContentModel{Languages: c.Languages, Tiers: c.Tiers, Kinds: c.Kinds, GroupKinds: c.GroupKinds, ContentFilters: c.ContentFilters, Signals: signals}
	if m.Languages == nil {
		m.Languages = []string{}
	}
	if m.Tiers == nil {
		m.Tiers = []string{}
	}
	if m.Kinds == nil {
		m.Kinds = map[string]kinds.Kind{}
	}
	if m.GroupKinds == nil {
		m.GroupKinds = map[string]kinds.GroupKind{}
	}
	if m.ContentFilters == nil {
		m.ContentFilters = []kinds.ContentFilter{}
	}
	return m
}

type server struct{ body []byte }

// Handler serves GET /kinds from c and whether signals are recorded,
// rendered once.
func Handler(c kinds.Config, signals bool) (http.Handler, error) {
	body, err := json.Marshal(Public(c, signals))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	httpapi.Mount(mux, nil, &server{body: body}, routes)
	return mux, nil
}

func init() { httpapi.Register(httpapi.Kinds, routes) }

var routes = []httpapi.Route[*server]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/kinds", Resource: "kinds", Auth: httpapi.Open,
		Doc:       "The site's content model: languages, tiers, content kinds, group kinds, content filters and whether signals are recorded. Cacheable for five minutes.",
		Responses: []httpapi.Reply{httpapi.OK(ContentModel{})}}, Serve: httpapi.H((*server).handle)},
}

func (s *server) handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.body)
}
