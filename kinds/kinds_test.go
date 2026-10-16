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
		Restrictions: []string{"restricted"},
		Domains: []Domain{
			{Name: "main", Hosts: []string{"doujins.com"}, Filters: []ContentFilter{{Group: GroupRef{"tag", "big-breasts"}}}},
			{Name: "gay", Hosts: []string{"doujins.gay"}, RequireGroups: []GroupRef{{"tag", "yaoi"}}},
			{Name: "moe", Hosts: []string{"hmovie-moe.us"}, ExcludeRestrictions: []string{"restricted"}},
		},
		Kinds: map[string]Kind{
			"gallery": {Route: "g", Presentation: "reader", Segments: true, Locked: &Locked{Show: []string{ShowCover, ShowGroups}, Preview: &Preview{Pages: 3}},
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
		"unknown restriction": func(c *Config) { c.Domains[2].ExcludeRestrictions = []string{"nsfw"} },
		"bad presentation":    func(c *Config) { k := c.Kinds["blog"]; k.Presentation = "page"; c.Kinds["blog"] = k },
		"filter group kind":   func(c *Config) { c.Domains[0].Filters[0].Group.Kind = "genre" },
		"enum without values": func(c *Config) { c.Kinds["gallery"].Attributes["format"] = Attribute{Type: "enum"} },
		"cap on a site kind":  func(c *Config) { c.GroupKinds["tag"] = GroupKind{Route: "tags", MaxPerOwner: 3} },
		"negative cap":        func(c *Config) { c.GroupKinds["channel"] = GroupKind{Owner: "user", MaxPerOwner: -1} },
		"site kind uploads":   func(c *Config) { c.GroupKinds["tag"] = GroupKind{Route: "tags", Uploads: UploadLimits{QuotaBytes: 1}} },
		"empty preview":       func(c *Config) { c.Kinds["gallery"].Locked.Preview = &Preview{} },
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
	if l := c.Kinds["gallery"].LockedView(); l.Shows(ShowDescription) || !l.Shows(ShowGroups) || l.Preview.Pages != 3 {
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
		t.Fatalf("unmatched host -> %s, want the first site", s.Name)
	}
}

func TestDomainContentFilters(t *testing.T) {
	c := doujins()
	off := false
	c.Domains[1].Filters = []ContentFilter{{Group: GroupRef{"tag", "gore"}, Visible: false, UserToggle: &off}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Domains[0].Filters[0].Toggleable() {
		t.Fatal("omitted user_toggle must allow overrides")
	}
	if c.Domains[1].Filters[0].Toggleable() {
		t.Fatal("false user_toggle must lock the rule")
	}
	if d, _ := c.DomainFor("DOUJINS.GAY:443"); d.Name != "gay" || len(d.Filters) != 1 {
		t.Fatalf("domain: %+v", d)
	}
	if d, _ := c.DomainFor("unknown.test"); d.Name != "main" {
		t.Fatalf("fallback: %+v", d)
	}
}

func TestRejectInvalidDomainFilters(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"repeated filter":                func(c *Config) { c.Domains[0].Filters = append(c.Domains[0].Filters, c.Domains[0].Filters[0]) },
		"repeated domain":                func(c *Config) { c.Domains[1].Name = c.Domains[0].Name },
		"unknown required kind":          func(c *Config) { c.Domains[1].RequireKinds = []string{"missing"} },
		"repeated required kind":         func(c *Config) { c.Domains[1].RequireKinds = []string{"gallery", "gallery"} },
		"required kind without entities": func(c *Config) { c.Domains[0].RequireKinds = []string{"gallery"} },
		"repeated host":                  func(c *Config) { c.Domains[1].Hosts = []string{"DOUJINS.COM:443"} },
		"required and hidden": func(c *Config) {
			c.Domains[1].Filters = []ContentFilter{{Group: GroupRef{"tag", "yaoi"}, Visible: false}}
		},
		"repeated required": func(c *Config) {
			c.Domains[1].RequireGroups = append(c.Domains[1].RequireGroups, c.Domains[1].RequireGroups[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := doujins()
			mutate(&c)
			if err := c.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v", err)
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

func TestRoutes(t *testing.T) {
	r := doujins().Routes()
	if r["gallery"] != "g" || r["tag"] != "tags" || r["voice_actor"] != "voice-actors" || len(r) != 6 {
		t.Fatalf("routes %v", r)
	}
}
