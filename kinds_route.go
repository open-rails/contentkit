package contentkit

// GET /v1/kinds serves the site's public content model: the part of
// kinds.Config clients render content and entity kinds with. Domain policy,
// restrictions and token claims stay on the server.

import (
	"encoding/json"
	"net/http"

	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// ContentModel is the public part of kinds.Config: Client.GetKinds.
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

// publicModel is c's public part, with empty lists and maps rather than null
// and each kind's locked view spelled out.
func publicModel(c kinds.Config, signals bool, avatars *media.PresetRule) ContentModel {
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

type kindsServer struct {
	content kinds.Config
	avatars *media.PresetRule
	signals func() bool
}

// kindsHandler serves the content model and the current domain’s configured buckets.
func kindsHandler(c kinds.Config, signals func() bool, avatars *media.PresetRule) http.Handler {
	srv := &kindsServer{content: c, signals: signals, avatars: avatars}
	mux := http.NewServeMux()
	httpapi.Mount(mux, nil, srv, kindsRoutes)
	return mux
}

func init() { httpapi.Register(httpapi.Kinds, kindsRoutes) }

var kindsRoutes = []httpapi.Route[*kindsServer]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/kinds", Resource: "kinds", Auth: httpapi.Open,
		Doc:       "The site's content model: languages, tiers, content kinds, entity kinds, whether signals are recorded now and people's avatar preset. Cacheable for a minute.",
		Responses: []httpapi.Reply{httpapi.OK(ContentModel{})}}, Serve: httpapi.H((*kindsServer).handle)},
}

func (s *kindsServer) handle(w http.ResponseWriter, r *http.Request) {
	model := publicModel(s.content, s.signals(), s.avatars)
	domain, _ := s.content.DomainFor(r.Host)
	for name, kind := range model.Kinds {
		buckets := make([]kinds.PopularityBucket, 0, len(kind.Popularity))
		for _, bucket := range kind.Popularity {
			scopes := make([]kinds.PopularityScope, 0, len(bucket.Scopes))
			for _, scope := range bucket.ActivityScopes() {
				if scope.Domain == "" || scope.Domain == domain.Name {
					scopes = append(scopes, scope)
				}
			}
			if len(scopes) > 0 {
				bucket.Scopes = scopes
				buckets = append(buckets, bucket)
			}
		}
		kind.Popularity = buckets
		model.Kinds[name] = kind
	}
	w.Header().Set("Content-Type", "application/json")
	// Short, so clients see the signal plane come and go.
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(model)
}
