package kinds

import (
	"errors"
	"slices"
	"testing"
)

func doujins() Config {
	return Config{
		Languages:    []string{"en", "es", "ja"},
		Tiers:        []string{"premium"},
		TokenClaims:  []string{"premium"},
		Restrictions: []string{"restricted"}, ContentFilters: ContentFilters{AppliesTo: []string{"gallery", "blog"}, Domains: []Domain{
			{Name: "main", Hosts: []string{"doujins.com"}, Filters: []ContentFilter{{Entity: EntityRef{"tag", "big-breasts"}}}},
			{Name: "gay", Hosts: []string{"doujins.gay"}, RequireEntities: map[string][]EntityRef{"gallery": {{"tag", "yaoi"}}}},
			{Name: "moe", Hosts: []string{"hmovie-moe.us"}, Filters: []ContentFilter{{Entity: EntityRef{"tag", "restricted"}, Visible: false, UserToggle: func() *bool { b := false; return &b }()}}},
		}},
		Kinds: map[string]Kind{
			"gallery": {Route: "g", Presentation: "reader", Segments: true, Locked: &Locked{Show: []string{ShowCover, ShowEntities}, Preview: &Preview{Pages: 3}},
				Versions:   VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Entities:   []string{"tag", "series", "character", "voice_actor"},
				Attributes: map[string]Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}, Filterable: true}}},
			"blog": {Route: "blog", Presentation: "post", Entities: []string{"tag"}, Versions: VersionRules{Languages: "one", Hidden: true}},
		},
		EntityKinds: map[string]EntityKind{
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
			k.Entities = append(k.Entities, "studio")
			c.Kinds["gallery"] = k
		},
		"duplicate route":  func(c *Config) { c.EntityKinds["tag"] = EntityKind{Route: "g"} },
		"claim not a tier": func(c *Config) { c.TokenClaims = []string{"gold"} },
		"unknown policy kind": func(c *Config) {
			c.ContentFilters.Domains[2].RequireEntities = map[string][]EntityRef{"missing": {{"tag", "yaoi"}}}
		},
		"bad presentation":    func(c *Config) { k := c.Kinds["blog"]; k.Presentation = "page"; c.Kinds["blog"] = k },
		"filter group kind":   func(c *Config) { c.ContentFilters.Domains[0].Filters[0].Entity.Kind = "genre" },
		"enum without values": func(c *Config) { c.Kinds["gallery"].Attributes["format"] = Attribute{Type: "enum"} },
		"cap on a site kind":  func(c *Config) { c.EntityKinds["tag"] = EntityKind{Route: "tags", MaxPerOwner: 3} },
		"negative cap":        func(c *Config) { c.EntityKinds["channel"] = EntityKind{Owner: "user", MaxPerOwner: -1} },
		"site kind uploads": func(c *Config) {
			c.EntityKinds["tag"] = EntityKind{Route: "tags", Uploads: UploadLimits{QuotaBytes: 1}}
		},
		"empty preview": func(c *Config) { c.Kinds["gallery"].Locked.Preview = &Preview{} },
		"segmentless preview": func(c *Config) {
			k := c.Kinds["blog"]
			k.Locked = &Locked{Preview: &Preview{Segments: 1}}
			c.Kinds["blog"] = k
		},
		"unknown property":  func(c *Config) { c.Kinds["gallery"].Locked.Show = []string{"body"} },
		"repeated property": func(c *Config) { c.Kinds["gallery"].Locked.Show = []string{ShowCover, ShowCover} },
	} {
		c := doujins()
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

// downloads is doujins with a movie kind serving an MP4.
func downloads() Config {
	c := doujins()
	c.Kinds["movie"] = Kind{Route: "watch", Presentation: "player", Versions: VersionRules{Languages: "many"}, Download: &Download{Serve: "mp4-1080"}}
	return c
}

func TestValidateDownloads(t *testing.T) {
	if err := downloads().Validate(); err != nil {
		t.Fatalf("downloads config: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"off and serving": func(c *Config) { c.Kinds["movie"].Download.Off = true },
		"serve a path":    func(c *Config) { c.Kinds["movie"].Download.Serve = "video/source.mp4" },
	} {
		c := downloads()
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestServes(t *testing.T) {
	c := downloads()
	for name, want := range map[string]string{"gallery": DownloadPages, "blog": "", "movie": "mp4-1080"} {
		if got := c.Kinds[name].Serves(); got != want {
			t.Errorf("%s serves %q, want %q", name, got, want)
		}
	}
	movie := c.Kinds["movie"]
	movie.Download = nil
	if movie.Serves() != DownloadOriginal {
		t.Fatalf("a player serves %q by default", movie.Serves())
	}
	movie.Download = &Download{Off: true}
	if movie.Serves() != "" {
		t.Fatalf("an off download serves %q", movie.Serves())
	}
}

func TestLockedView(t *testing.T) {
	c := doujins()
	if l := c.Kinds["blog"].LockedView(); !l.Shows(ShowCover) || l.Shows(ShowComments) || l.Shows(ShowReactions) || l.Preview != nil {
		t.Fatalf("default locked view %+v", l)
	}
	if l := c.Kinds["gallery"].LockedView(); l.Shows(ShowDescription) || !l.Shows(ShowEntities) || l.Preview.Pages != 3 {
		t.Fatalf("configured locked view %+v", l)
	}
	k := c.Kinds["blog"]
	k.Locked = &Locked{}
	if l := k.LockedView(); slices.ContainsFunc(Shown, l.Shows) {
		t.Fatalf("an empty locked view shows %+v", l)
	}
}

func TestDomainFor(t *testing.T) {
	c := doujins()
	if s, _ := c.DomainFor("doujins.gay"); s.Name != "gay" {
		t.Fatalf("doujins.gay -> %s", s.Name)
	}
	if s, _ := c.DomainFor("localhost"); s.Name != "main" {
		t.Fatalf("unmatched host -> %s, want main", s.Name)
	}
}

func TestDomainContentFilters(t *testing.T) {
	c := doujins()
	off := false
	c.ContentFilters.Domains[1].Filters = []ContentFilter{{Entity: EntityRef{"tag", "gore"}, Visible: false, UserToggle: &off}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.ContentFilters.Domains[0].Filters[0].Toggleable() || c.ContentFilters.Domains[1].Filters[0].Toggleable() {
		t.Fatal("toggle defaults changed")
	}
	if d, _ := c.DomainFor("DOUJINS.GAY:443"); d.Name != "gay" || len(d.Filters) != 1 {
		t.Fatalf("domain%+v", d)
	}
	if d, _ := c.DomainFor("unknown.test"); d.Name != "main" {
		t.Fatalf("fallback%+v", d)
	}
}
func TestContentKindsDoNotConflictWithDomainMetadata(t *testing.T) {
	for _, name := range []string{"name", "hosts"} {
		t.Run(name, func(t *testing.T) {
			c := doujins()
			k := c.Kinds["gallery"]
			k.Route = name
			c.Kinds[name] = k
			if err := c.Validate(); err != nil {
				t.Fatalf("separate content kind namespace: %v", err)
			}
		})
	}
}
func TestRejectInvalidDomainFilters(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"repeated filter": func(c *Config) { d := &c.ContentFilters.Domains[0]; d.Filters = append(d.Filters, d.Filters[0]) },
		"repeated domain": func(c *Config) { c.ContentFilters.Domains[1].Name = c.ContentFilters.Domains[0].Name },
		"unknown required content kind": func(c *Config) {
			c.ContentFilters.Domains[1].RequireEntities = map[string][]EntityRef{"missing": {{"tag", "yaoi"}}}
		},
		"repeated host": func(c *Config) { c.ContentFilters.Domains[1].Hosts = []string{"DOUJINS.COM:443"} },
		"required and hidden": func(c *Config) {
			c.ContentFilters.Domains[1].Filters = []ContentFilter{{Entity: EntityRef{"tag", "yaoi"}, Visible: false}}
		},
		"repeated required": func(c *Config) {
			c.ContentFilters.Domains[1].RequireEntities["gallery"] = []EntityRef{{"tag", "yaoi"}, {"tag", "yaoi"}}
		},
		"no applies_to":             func(c *Config) { c.ContentFilters.AppliesTo = nil },
		"unknown applies_to":        func(c *Config) { c.ContentFilters.AppliesTo = []string{"missing"} },
		"repeated applies_to":       func(c *Config) { c.ContentFilters.AppliesTo = []string{"gallery", "gallery"} },
		"unsupported shared entity": func(c *Config) { k := c.Kinds["blog"]; k.Entities = nil; c.Kinds["blog"] = k },
		"unsupported required entity": func(c *Config) {
			c.ContentFilters.Domains[1].RequireEntities["gallery"] = []EntityRef{{"voice_actor", "actor"}}
			k := c.Kinds["gallery"]
			k.Entities = []string{"tag"}
			c.Kinds["gallery"] = k
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := doujins()
			mutate(&c)
			if err := c.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got%v", err)
			}
		})
	}
}

func TestValidateAttributes(t *testing.T) {
	declared := map[string]Attribute{
		"format": {Type: "enum", Values: []string{"doujin", "manga"}},
		"pages":  {Type: "int"},
		"link":   {Type: "string"},
		"adult":  {Type: "bool"},
		"born":   {Type: "time"},
	}
	if _, err := ValidateAttributes(declared, map[string]any{"link2": "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("undeclared attribute: %v", err)
	}
	got, err := ValidateAttributes(declared, map[string]any{"format": "manga", "pages": float64(12), "link": "x", "adult": true, "born": "2020-01-02T03:04:05+02:00"})
	if err != nil || got["pages"] != int64(12) || got["born"] != "2020-01-02T01:04:05Z" || len(got) != 5 {
		t.Fatalf("normalized %v %v", got, err)
	}
	if got, err := ValidateAttributes(declared, map[string]any{"link": nil}); err != nil || len(got) != 0 {
		t.Fatalf("nil drops: %v %v", got, err)
	}
	for _, bad := range []map[string]any{{"format": "comic"}, {"pages": 1.5}, {"pages": "1"}, {"adult": "yes"}, {"born": "yesterday"}, {"link": 3.0}} {
		if _, err := ValidateAttributes(declared, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v accepted: %v", bad, err)
		}
	}
}

func TestRequiredAttributes(t *testing.T) {
	declared := map[string]Attribute{"format": {Type: "enum", Values: []string{"doujin", "cgset"}, Required: true}}
	for _, values := range []map[string]any{nil, {}, {"format": nil}, {"format": ""}, {"format": "  "}} {
		if err := ValidateRequiredAttributes(declared, values); !errors.Is(err, ErrInvalid) {
			t.Fatalf("missing required value %v accepted: %v", values, err)
		}
	}
	if err := ValidateRequiredAttributes(declared, map[string]any{"format": "cgset"}); err != nil {
		t.Fatal(err)
	}
	// Allocation drafts and version overrides are intentionally allowed to be
	// incomplete; only their supplied values are type-checked here.
	if _, err := ValidateAttributes(declared, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequiredAttributes(map[string]Attribute{"enabled": {Type: "bool", Required: true}, "count": {Type: "int", Required: true}}, map[string]any{"enabled": false, "count": 0}); err != nil {
		t.Fatalf("false and zero are present values: %v", err)
	}
}

func TestKindAccessScope(t *testing.T) {
	for _, scope := range []AccessScope{"", AccessContent, AccessVersion, "other"} {
		c := doujins()
		kind := c.Kinds["gallery"]
		kind.AccessScope = scope
		c.Kinds["gallery"] = kind
		err := c.Validate()
		if (scope == "other") != errors.Is(err, ErrInvalid) {
			t.Fatalf("scope %q: %v", scope, err)
		}
	}
}

func TestRoutes(t *testing.T) {
	r := doujins().Routes()
	if r["gallery"] != "g" || r["tag"] != "tags" || r["voice_actor"] != "voice-actors" || len(r) != 6 {
		t.Fatalf("routes %v", r)
	}
}

func TestContentFiltersCloneDoesNotAliasConfiguration(t *testing.T) {
	no := false
	original := ContentFilters{AppliesTo: []string{"gallery", "blog"}, Domains: []Domain{{Name: "main", Hosts: []string{"example.test"}, RequireEntities: map[string][]EntityRef{"gallery": {{"tag", "yaoi"}}}, Filters: []ContentFilter{{Entity: EntityRef{"tag", "restricted"}, UserToggle: &no}}}}}
	cloned := original.Clone()
	original.AppliesTo[0] = "changed"
	original.Domains[0].Name = "changed"
	original.Domains[0].Hosts[0] = "changed.test"
	original.Domains[0].RequireEntities["gallery"][0].Slug = "changed"
	original.Domains[0].Filters[0].Entity.Slug = "changed"
	*original.Domains[0].Filters[0].UserToggle = true
	if cloned.AppliesTo[0] != "gallery" || cloned.Domains[0].Name != "main" || cloned.Domains[0].Hosts[0] != "example.test" || cloned.Domains[0].RequireEntities["gallery"][0].Slug != "yaoi" || cloned.Domains[0].Filters[0].Entity.Slug != "restricted" || *cloned.Domains[0].Filters[0].UserToggle {
		t.Fatalf("mutable source changed snapshot %+v", cloned)
	}
}

func TestMainFallbackIsIndependentOfDomainOrder(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {1, 0, 2}, {1, 2, 0}} {
		cfg := doujins()
		original := slices.Clone(cfg.ContentFilters.Domains)
		for index, source := range order {
			cfg.ContentFilters.Domains[index] = original[source]
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		for host, expected := range map[string]string{"": "main", "unknown.test": "main", "doujins.com": "main", "DOUJINS.GAY.:443": "gay", "hmovie-moe.us": "moe"} {
			if domain, ok := cfg.DomainFor(host); !ok || domain.Name != expected {
				t.Fatalf("order %v, host %q: %+v %v", order, host, domain, ok)
			}
		}
	}
}

func TestMainFallbackValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no main":                   func(c *Config) { c.ContentFilters.Domains[0].Name = "renamed" },
		"two main":                  func(c *Config) { c.ContentFilters.Domains[1].Name = "main" },
		"hostless alternate first":  func(c *Config) { c.ContentFilters.Domains[1].Hosts = nil; slices.Reverse(c.ContentFilters.Domains) },
		"ambiguous normalized host": func(c *Config) { c.ContentFilters.Domains[1].Hosts = []string{" DOUJINS.COM.:443 "} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := doujins()
			mutate(&cfg)
			if err := cfg.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid domains accepted: %v", err)
			}
		})
	}
	cfg := doujins()
	cfg.ContentFilters.Domains[0].Hosts = nil
	slices.Reverse(cfg.ContentFilters.Domains)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("main may omit aliases wherever placed: %v", err)
	}
	cfg.ContentFilters.Domains = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("optional domain policy cannot be disabled: %v", err)
	}
	if _, ok := cfg.DomainFor("unknown.test"); ok {
		t.Fatal("disabled policy returned a fallback")
	}
}
