package media

import "github.com/open-rails/contentkit/media/layout"

// AgentRules are the access agent's rules for an app's media host: the
// namespaces it serves there (MEDIA_ACCESS_HOSTS, with
// layout.FormatHosts) and the default public names of its kinds
// (MEDIA_ACCESS_DEFAULTS, with layout.FormatDefaults).
type AgentRules struct {
	Namespaces []string
	Defaults   []layout.Default
}

// AgentConfig derives the access agent's rules from the registry.
func AgentConfig(r *Registry) AgentRules {
	out := AgentRules{Namespaces: r.Namespaces()}
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
