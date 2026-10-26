// Package kindsroute serves the site's public content model: the part of
// kinds.Config clients render content and entity kinds with.
// Domain policy, restrictions and token claims stay on the server.
package kindsroute

import (
	"encoding/json"
	"net/http"

	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// ContentModel is the public part of kinds.Config.
type ContentModel struct {
	Channels    *kinds.Channels             `json:"channels,omitempty"`
	Languages   []string                    `json:"languages"`
	Tiers       []string                    `json:"tiers"`
	Kinds       map[string]kinds.Kind       `json:"kinds"`
	EntityKinds map[string]kinds.EntityKind `json:"entity_kinds"`
	// Signals: the site records signals and exposures now. Without, clients
	// send none and read the model again later: a configured signal plane
	// that is down comes back (history, similar and recommendations answer
	// unavailable meanwhile), one never configured answers not_configured.
	Signals bool `json:"signals"`
	// Avatars is people's avatar preset (Stable): an avatar's URLs are its
	// names at {base}/v1/{namespace}/{kind}/{user id}/public/, and its
	// uploads target {kind, user id}. Absent: people have none.
	Avatars *media.PresetRule `json:"avatars,omitempty"`
}

// Public is c's public part, with empty lists and maps rather than null and
// each kind's locked view spelled out.
func Public(c kinds.Config, signals bool, avatars *media.PresetRule) ContentModel {
	m := ContentModel{Channels: c.Channels, Languages: c.Languages, Tiers: c.Tiers, Kinds: map[string]kinds.Kind{}, EntityKinds: c.EntityKinds,
		Signals: signals, Avatars: avatars}
	for name, k := range c.Kinds {
		l := k.LockedView()
		if l.Show == nil {
			l.Show = []string{}
		}
		k.Locked = &l
		m.Kinds[name] = k
	}
	if m.Languages == nil {
		m.Languages = []string{}
	}
	if m.Tiers == nil {
		m.Tiers = []string{}
	}
	if m.EntityKinds == nil {
		m.EntityKinds = map[string]kinds.EntityKind{}
	}
	return m
}

type server struct {
	bodies  [2][]byte // without and with signals
	signals func() bool
}

// Handler serves GET /kinds from c and the avatar preset, rendered once,
// and whether signals are recorded, asked per request.
func Handler(c kinds.Config, signals func() bool, avatars *media.PresetRule) (http.Handler, error) {
	srv := &server{signals: signals}
	for i, on := range []bool{false, true} {
		body, err := json.Marshal(Public(c, on, avatars))
		if err != nil {
			return nil, err
		}
		srv.bodies[i] = body
	}
	mux := http.NewServeMux()
	httpapi.Mount(mux, nil, srv, routes)
	return mux, nil
}

func init() { httpapi.Register(httpapi.Kinds, routes) }

var routes = []httpapi.Route[*server]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/kinds", Resource: "kinds", Auth: httpapi.Open,
		Doc:       "The site's content model: languages, tiers, content kinds, entity kinds, whether signals are recorded now and people's avatar preset. Cacheable for a minute.",
		Responses: []httpapi.Reply{httpapi.OK(ContentModel{})}}, Serve: httpapi.H((*server).handle)},
}

func (s *server) handle(w http.ResponseWriter, _ *http.Request) {
	body := s.bodies[0]
	if s.signals() {
		body = s.bodies[1]
	}
	w.Header().Set("Content-Type", "application/json")
	// Short, so clients see the signal plane come and go.
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write(body)
}
