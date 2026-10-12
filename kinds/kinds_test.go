package kinds

import (
	"errors"
	"testing"
)

func doujins() Config {
	return Config{
		Languages:    []string{"en", "es", "ja"},
		Tiers:        []string{"premium"},
		TokenClaims:  []string{"premium"},
		Restrictions: []string{"restricted"},
		Sites: []Site{
			{Name: "main", Hosts: []string{"doujins.com"}},
			{Name: "gay", Hosts: []string{"doujins.gay"}, RequireGroups: []GroupRef{{"tag", "yaoi"}}},
			{Name: "moe", Hosts: []string{"hmovie-moe.us"}, ExcludeRestrictions: []string{"restricted"}},
		},
		ContentFilters: []ContentFilter{{Group: GroupRef{"tag", "big-breasts"}}},
		Kinds: map[string]Kind{
			"gallery": {Route: "g", Presentation: "reader", Segments: true,
				Versions:   VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Groups:     []string{"tag", "series", "character", "voice_actor"},
				Attributes: map[string]Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}, Filterable: true}}},
			"blog": {Route: "blog", Presentation: "post", Versions: VersionRules{Languages: "one", Hidden: true}},
		},
		GroupKinds: map[string]GroupKind{
			"tag":         {Route: "tags"},
			"series":      {Route: "series", Ordered: true, Parent: "series"},
			"character":   {Route: "characters", ScopedBy: "series"},
			"voice_actor": {Route: "voice-actors", VersionLevel: true},
		},
	}
}

func TestValidate(t *testing.T) {
	if err := doujins().Validate(); err != nil {
		t.Fatalf("doujins config: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no languages": func(c *Config) { c.Languages = nil },
		"unknown group kind": func(c *Config) {
			k := c.Kinds["gallery"]
			k.Groups = append(k.Groups, "studio")
			c.Kinds["gallery"] = k
		},
		"duplicate route":     func(c *Config) { c.GroupKinds["tag"] = GroupKind{Route: "g"} },
		"claim not a tier":    func(c *Config) { c.TokenClaims = []string{"gold"} },
		"unknown restriction": func(c *Config) { c.Sites[2].ExcludeRestrictions = []string{"nsfw"} },
		"bad presentation":    func(c *Config) { k := c.Kinds["blog"]; k.Presentation = "page"; c.Kinds["blog"] = k },
		"filter group kind":   func(c *Config) { c.ContentFilters[0].Group.Kind = "genre" },
		"enum without values": func(c *Config) { c.Kinds["gallery"].Attributes["format"] = Attribute{Type: "enum"} },
	} {
		c := doujins()
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestSiteFor(t *testing.T) {
	c := doujins()
	if s, _ := c.SiteFor("doujins.gay"); s.Name != "gay" {
		t.Fatalf("doujins.gay -> %s", s.Name)
	}
	if s, _ := c.SiteFor("localhost"); s.Name != "main" {
		t.Fatalf("unmatched host -> %s, want the first site", s.Name)
	}
}
