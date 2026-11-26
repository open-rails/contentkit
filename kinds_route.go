package contentkit

// GET /v1/kinds serves the site's public content model: the part of
// kinds.Config clients render content and entity kinds with. Domain policy,
// restrictions and token claims stay on the server.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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
	// Signals: the site logs activity (it has a signal plane). Without,
	// clients send none: POST /v1/activity answers not_configured.
	Signals bool `json:"signals"`
	// Avatars is people's avatar preset with its default image; uploads
	// target {kind, user id}. Absent: people have none. Another person's
	// avatar is listed beside what they wrote.
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
	avatars func(context.Context) *media.PresetRule
	signals func() bool
}

// kindsHandler serves the content model and the current domain’s configured buckets.
func kindsHandler(guard httpapi.Guard, c kinds.Config, signals func() bool, avatars func(context.Context) *media.PresetRule) http.Handler {
	srv := &kindsServer{content: c, signals: signals, avatars: avatars}
	mux := http.NewServeMux()
	httpapi.Mount(mux, guard, srv, kindsRoutes)
	return mux
}

func init() { httpapi.Register(httpapi.Kinds, kindsRoutes) }

var kindsRoutes = []httpapi.Route[*kindsServer]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/kinds", Resource: "kinds", Auth: httpapi.Open,
		Doc:       "The site's content model: languages, tiers, content kinds, entity kinds, whether activity is logged and people's avatar preset.",
		Responses: []httpapi.Reply{httpapi.OK(ContentModel{})}, Cache: httpapi.Cache{MaxAge: time.Minute}}, Serve: httpapi.H((*kindsServer).handle)},
}

func (s *kindsServer) handle(w http.ResponseWriter, r *http.Request) {
	model := publicModel(s.content, s.signals(), s.avatars(r.Context()))
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
	_ = json.NewEncoder(w).Encode(model)
}
