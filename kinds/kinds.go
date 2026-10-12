// Package kinds is the site's content model as configuration: content kinds,
// group kinds, tiers, restrictions, site scopes and content filters. One
// Config drives storage, routes, media kinds, search and URLs (ContentKit #117).
package kinds

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"
)

// ErrInvalid is a Config that cannot be served.
var ErrInvalid = errors.New("kinds: invalid config")

var (
	identRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	routeRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	languageRE = regexp.MustCompile(`^[a-z]{2,3}(-[a-zA-Z0-9]{2,8})*$`)
)

// Config is the site's content model.
type Config struct {
	Languages      []string             `json:"languages" yaml:"languages"`
	Tiers          []string             `json:"tiers,omitempty" yaml:"tiers"`
	TokenClaims    []string             `json:"token_claims,omitempty" yaml:"token_claims"`
	Restrictions   []string             `json:"restrictions,omitempty" yaml:"restrictions"`
	Sites          []Site               `json:"sites,omitempty" yaml:"sites"`
	ContentFilters []ContentFilter      `json:"content_filters,omitempty" yaml:"content_filters"`
	Kinds          map[string]Kind      `json:"kinds" yaml:"kinds"`
	GroupKinds     map[string]GroupKind `json:"group_kinds,omitempty" yaml:"group_kinds"`
}

// Site is one deployment's slice of the catalog, matched by request host.
// The first site is the default for unmatched hosts.
type Site struct {
	Name                string     `json:"name" yaml:"name"`
	Hosts               []string   `json:"hosts" yaml:"hosts"`
	ExcludeRestrictions []string   `json:"exclude_restrictions,omitempty" yaml:"exclude_restrictions"`
	RequireGroups       []GroupRef `json:"require_groups,omitempty" yaml:"require_groups"`
}

// GroupRef names a group by kind and slug.
type GroupRef struct {
	Kind string `json:"kind" yaml:"kind"`
	Slug string `json:"slug" yaml:"slug"`
}

// ContentFilter is a group viewers may hide; DefaultVisible is the default
// before a viewer's own override.
type ContentFilter struct {
	Group          GroupRef          `json:"group" yaml:"group"`
	DefaultVisible bool              `json:"default_visible" yaml:"default_visible"`
	Labels         map[string]string `json:"labels,omitempty" yaml:"labels"`
}

// Kind is one content kind: a gallery, a movie, a creator post, a blog post.
type Kind struct {
	// Route is the URL segment: /{lang}/{route}/{CODE}/{slug}.
	Route string `json:"route" yaml:"route"`
	// Presentation tells contentkit-ui how to show it: reader, player or post.
	Presentation string       `json:"presentation" yaml:"presentation"`
	Versions     VersionRules `json:"versions" yaml:"versions"`
	// Segments enables ordered, individually released ranges (chapters).
	Segments bool `json:"segments,omitempty" yaml:"segments"`
	// Groups are the group kinds content of this kind may join.
	Groups     []string             `json:"groups,omitempty" yaml:"groups"`
	Attributes map[string]Attribute `json:"attributes,omitempty" yaml:"attributes"`
	// Sort is the default list order.
	Sort string `json:"sort,omitempty" yaml:"sort"`
}

// VersionRules shape a kind's versions.
type VersionRules struct {
	// Languages is "one" (a version serves one language) or "many".
	Languages string `json:"languages" yaml:"languages"`
	// Traits are the declared version traits (colored, uncensored, ...).
	Traits []string `json:"traits,omitempty" yaml:"traits"`
	// Hidden kinds have exactly one version that clients never see.
	Hidden bool `json:"hidden,omitempty" yaml:"hidden"`
}

// Attribute is one declared per-kind field.
type Attribute struct {
	// Type is string, int, bool, enum or time.
	Type       string   `json:"type" yaml:"type"`
	Values     []string `json:"values,omitempty" yaml:"values"`
	Filterable bool     `json:"filterable,omitempty" yaml:"filterable"`
	Sortable   bool     `json:"sortable,omitempty" yaml:"sortable"`
}

// GroupKind is one kind of grouping: tag, artist, series, channel, rail, ...
type GroupKind struct {
	Route string `json:"route,omitempty" yaml:"route"`
	// Ordered members carry positions (series, playlists, rails).
	Ordered bool `json:"ordered,omitempty" yaml:"ordered"`
	// Parent is the group kind a group may sit under (series under series).
	Parent string `json:"parent,omitempty" yaml:"parent"`
	// ScopedBy makes slugs unique within a group of that kind (character
	// within series).
	ScopedBy string `json:"scoped_by,omitempty" yaml:"scoped_by"`
	// Owner is "site" (staff-managed) or "user" (channels, playlists): a
	// signed-in person creates a user-owned group and owns it.
	Owner string `json:"owner,omitempty" yaml:"owner"`
	// MaxPerOwner caps the active groups of the kind one person owns; 0 is
	// no cap. Staff are not capped.
	MaxPerOwner int `json:"max_per_owner,omitempty" yaml:"max_per_owner"`
	// Sellable groups carry offers; their key unlocks every member.
	Sellable bool `json:"sellable,omitempty" yaml:"sellable"`
	// AuthScope groups name an AuthKit group whose permissions govern their
	// content (OnlyDemo channels).
	AuthScope bool `json:"auth_scope,omitempty" yaml:"auth_scope"`
	// Windows lets memberships carry active_from/active_until (rails).
	Windows bool `json:"windows,omitempty" yaml:"windows"`
	// VersionLevel lets memberships target one version (voice actors).
	VersionLevel bool `json:"version_level,omitempty" yaml:"version_level"`
	// Roles are membership roles (publisher on an artist group).
	Roles      []string             `json:"roles,omitempty" yaml:"roles"`
	Attributes map[string]Attribute `json:"attributes,omitempty" yaml:"attributes"`
}

var (
	presentations = []string{"reader", "player", "post"}
	attrTypes     = []string{"string", "int", "bool", "enum", "time"}
)

// Validate refuses a Config that names unknown kinds, languages, tiers or
// restrictions, or repeats a route.
func (c Config) Validate() error {
	if len(c.Languages) == 0 {
		return fmt.Errorf("%w: languages is required", ErrInvalid)
	}
	for _, l := range c.Languages {
		if !languageRE.MatchString(l) {
			return fmt.Errorf("%w: language %q", ErrInvalid, l)
		}
	}
	for _, list := range [][]string{c.Tiers, c.Restrictions} {
		for _, n := range list {
			if !identRE.MatchString(n) {
				return fmt.Errorf("%w: name %q must match %s", ErrInvalid, n, identRE)
			}
		}
	}
	for _, t := range c.TokenClaims {
		if !slices.Contains(c.Tiers, t) {
			return fmt.Errorf("%w: token claim %q is not a tier", ErrInvalid, t)
		}
	}
	if len(c.Kinds) == 0 {
		return fmt.Errorf("%w: kinds is required", ErrInvalid)
	}
	routes := map[string]string{}
	route := func(owner, r string) error {
		if r == "" {
			return nil
		}
		if !routeRE.MatchString(r) {
			return fmt.Errorf("%w: %s route %q", ErrInvalid, owner, r)
		}
		if prev, dup := routes[r]; dup {
			return fmt.Errorf("%w: route %q is used by %s and %s", ErrInvalid, r, prev, owner)
		}
		routes[r] = owner
		return nil
	}
	for name, gk := range c.GroupKinds {
		if !identRE.MatchString(name) {
			return fmt.Errorf("%w: group kind %q", ErrInvalid, name)
		}
		if _, clash := c.Kinds[name]; clash {
			return fmt.Errorf("%w: %q is both a kind and a group kind", ErrInvalid, name)
		}
		if err := route("group kind "+name, gk.Route); err != nil {
			return err
		}
		for _, ref := range []string{gk.Parent, gk.ScopedBy} {
			if _, ok := c.GroupKinds[ref]; ref != "" && !ok {
				return fmt.Errorf("%w: group kind %s names unknown group kind %q", ErrInvalid, name, ref)
			}
		}
		if gk.Owner != "" && gk.Owner != "site" && gk.Owner != "user" {
			return fmt.Errorf("%w: group kind %s owner %q", ErrInvalid, name, gk.Owner)
		}
		if gk.MaxPerOwner < 0 || gk.MaxPerOwner > 0 && gk.Owner != "user" {
			return fmt.Errorf("%w: group kind %s: max_per_owner is a non-negative cap of user-owned groups", ErrInvalid, name)
		}
		for _, r := range gk.Roles {
			if !identRE.MatchString(r) {
				return fmt.Errorf("%w: group kind %s role %q", ErrInvalid, name, r)
			}
		}
		if err := validateAttributes("group kind "+name, gk.Attributes); err != nil {
			return err
		}
	}
	for name, k := range c.Kinds {
		if !identRE.MatchString(name) {
			return fmt.Errorf("%w: kind %q", ErrInvalid, name)
		}
		if k.Route == "" {
			return fmt.Errorf("%w: kind %s needs a route", ErrInvalid, name)
		}
		if err := route("kind "+name, k.Route); err != nil {
			return err
		}
		if !slices.Contains(presentations, k.Presentation) {
			return fmt.Errorf("%w: kind %s presentation %q", ErrInvalid, name, k.Presentation)
		}
		if k.Versions.Languages != "one" && k.Versions.Languages != "many" {
			return fmt.Errorf("%w: kind %s versions.languages must be one or many", ErrInvalid, name)
		}
		for _, t := range k.Versions.Traits {
			if !identRE.MatchString(t) {
				return fmt.Errorf("%w: kind %s trait %q", ErrInvalid, name, t)
			}
		}
		for _, g := range k.Groups {
			if _, ok := c.GroupKinds[g]; !ok {
				return fmt.Errorf("%w: kind %s names unknown group kind %q", ErrInvalid, name, g)
			}
		}
		if err := validateAttributes("kind "+name, k.Attributes); err != nil {
			return err
		}
	}
	for _, r := range c.groupRefs() {
		if _, ok := c.GroupKinds[r.Kind]; !ok || r.Slug == "" {
			return fmt.Errorf("%w: group %s/%s names an unknown group kind or no slug", ErrInvalid, r.Kind, r.Slug)
		}
	}
	for i, s := range c.Sites {
		if s.Name == "" || (len(s.Hosts) == 0 && i > 0) {
			return fmt.Errorf("%w: site %d needs a name and hosts", ErrInvalid, i)
		}
		for _, r := range s.ExcludeRestrictions {
			if !slices.Contains(c.Restrictions, r) {
				return fmt.Errorf("%w: site %s excludes unknown restriction %q", ErrInvalid, s.Name, r)
			}
		}
	}
	return nil
}

func (c Config) groupRefs() []GroupRef {
	var out []GroupRef
	for _, s := range c.Sites {
		out = append(out, s.RequireGroups...)
	}
	for _, f := range c.ContentFilters {
		out = append(out, f.Group)
	}
	return out
}

func validateAttributes(owner string, attrs map[string]Attribute) error {
	for name, a := range attrs {
		if !identRE.MatchString(name) {
			return fmt.Errorf("%w: %s attribute %q", ErrInvalid, owner, name)
		}
		if !slices.Contains(attrTypes, a.Type) {
			return fmt.Errorf("%w: %s attribute %s type %q", ErrInvalid, owner, name, a.Type)
		}
		if (a.Type == "enum") != (len(a.Values) > 0) {
			return fmt.Errorf("%w: %s attribute %s: values belong to enums only", ErrInvalid, owner, name)
		}
	}
	return nil
}

// ValidateAttributes checks values against the declared attributes and returns
// them normalized: ints as int64, times as RFC 3339 UTC. A nil value drops the
// attribute; an undeclared one or a value of the wrong type is ErrInvalid.
func ValidateAttributes(declared map[string]Attribute, values map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(values))
	for name, v := range values {
		a, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("%w: attribute %q is not declared", ErrInvalid, name)
		}
		if v == nil {
			continue
		}
		n, err := a.normalize(v)
		if err != nil {
			return nil, fmt.Errorf("%w: attribute %s: %v", ErrInvalid, name, err)
		}
		out[name] = n
	}
	return out, nil
}

func (a Attribute) normalize(v any) (any, error) {
	switch a.Type {
	case "string":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "bool":
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case "int":
		switch n := v.(type) {
		case int:
			return int64(n), nil
		case int64:
			return n, nil
		case float64:
			if n == math.Trunc(n) && math.Abs(n) <= 1<<53 {
				return int64(n), nil
			}
		}
	case "enum":
		if s, ok := v.(string); ok && slices.Contains(a.Values, s) {
			return s, nil
		}
	case "time":
		switch t := v.(type) {
		case time.Time:
			return t.UTC().Format(time.RFC3339Nano), nil
		case string:
			if p, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return p.UTC().Format(time.RFC3339Nano), nil
			}
		}
	}
	return nil, fmt.Errorf("%v is not a valid %s", v, a.Type)
}

// Routes maps every content kind and group kind with a route to it: the
// first path segment of its pages (contenturl.Routes).
func (c Config) Routes() map[string]string {
	out := map[string]string{}
	for name, k := range c.Kinds {
		out[name] = k.Route
	}
	for name, gk := range c.GroupKinds {
		if gk.Route != "" {
			out[name] = gk.Route
		}
	}
	return out
}

// SiteFor returns the site serving host; the first site when none matches.
func (c Config) SiteFor(host string) (Site, bool) {
	for _, s := range c.Sites {
		if slices.Contains(s.Hosts, host) {
			return s, true
		}
	}
	if len(c.Sites) > 0 {
		return c.Sites[0], true
	}
	return Site{}, false
}
