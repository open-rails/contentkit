package mediastore

import (
	"slices"

	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// GatewayConfig combines the registry namespaces with the fallback mappings
// returned by image.PublishDefaults. Publish successfully before deploying them.
func GatewayConfig(r *Registry, defaults []layout.Default) media.GatewayRules {
	out := media.GatewayRules{Defaults: defaults}
	if scope, ok := r.Scope(); ok {
		out.Scopes = []layout.Scope{scope}
		for _, k := range r.Kinds() {
			if k.Account {
				account := scope
				account.AppID = ""
				if !slices.Contains(out.Scopes, account) {
					out.Scopes = append(out.Scopes, account)
				}
			}
		}
	} else {
		out.Namespaces = r.Namespaces()
	}
	return out
}
