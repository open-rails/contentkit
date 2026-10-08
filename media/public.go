package media

import "github.com/open-rails/contentkit/media/layout"

// GatewayRules are the media gateway's rules for an app's media host: the
// namespaces it serves there (MEDIA_GATEWAY_HOSTS, with
// layout.FormatHosts) and the default public names of its kinds
// (MEDIA_GATEWAY_DEFAULTS, with layout.FormatDefaults).
type GatewayRules struct {
	Namespaces []string
	Defaults   []layout.Default
}

// GatewayConfig derives the media gateway's rules from the registry.
func GatewayConfig(r *Registry) GatewayRules {
	out := GatewayRules{Namespaces: r.Namespaces()}
	for _, k := range r.cfg.Kinds {
		var names []string
		for _, p := range k.Public {
			if p.Default != "" {
				names = append(names, p.To)
			}
		}
		if len(names) > 0 {
			out.Defaults = append(out.Defaults, layout.Default{Namespace: k.ns, Kind: k.Name, Names: names})
		}
	}
	return out
}
