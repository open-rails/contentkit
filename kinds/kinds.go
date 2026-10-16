// Package kinds is the site's content model as configuration: content kinds,
// group kinds, tiers, restrictions, site scopes and content filters. One
// Config drives storage, routes, media kinds, search and URLs (ContentKit #117).
package kinds

import (
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strings"
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
	GroupKinds     map[string]GroupKind `json:"entity_kinds,omitempty" yaml:"group_kinds"`
}

// Site is one deployment's slice of the catalog, matched by request host.
// The first site is the default for unmatched hosts.
type Site struct {
	Name                string     `json:"name" yaml:"name"`
	Hosts               []string   `json:"hosts" yaml:"hosts"`
	ExcludeRestrictions []string   `json:"exclude_restrictions,omitempty" yaml:"exclude_restrictions"`
	RequireGroups       []GroupRef `json:"require_groups,omitempty" yaml:"require_groups"`
	// ContentFilterDefaults override app defaults for this entrypoint. A
	// reader's explicit saved preference still wins; these are not access rules.
	ContentFilterDefaults []ContentFilterDefault `json:"content_filter_defaults,omitempty" yaml:"content_filter_defaults"`
}

// GroupRef names a group by kind and slug.
type GroupRef struct {
	Kind string `json:"kind" yaml:"kind"`
	Slug string `json:"slug" yaml:"slug"`
}

// ContentFilter is a group viewers may hide; DefaultVisible is the default
// before a viewer's own override.
type ContentFilter struct {
	Group          GroupRef `json:"entity" yaml:"group"`
	DefaultVisible bool     `json:"default_visible" yaml:"default_visible"`
}

// ContentFilterDefault sets the visibility of one declared ContentFilter on
// a site. Unspecified filters retain their app-wide default.
type ContentFilterDefault struct {
	Group   GroupRef `json:"entity" yaml:"group"`
	Visible bool     `json:"visible" yaml:"visible"`
}

// DefaultVisible resolves a filter's default for this site before any saved
// reader override. It does not change RequireGroups or other access rules.
func (s Site) DefaultVisible(filter ContentFilter) bool {
	for _, d := range s.ContentFilterDefaults {
		if d.Group == filter.Group {
			return d.Visible
		}
	}
	return filter.DefaultVisible
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
	Groups     []string             `json:"entities,omitempty" yaml:"groups"`
	Attributes map[string]Attribute `json:"attributes,omitempty" yaml:"attributes"`
	// Sort is the default list order.
	Sort string `json:"sort,omitempty" yaml:"sort"`
	// Locked is what a viewer who does not open its content sees of it;
	// nil is DefaultLocked.
	Locked *Locked `json:"locked,omitempty" yaml:"locked"`
	// DraftTTL expires drafts that never went live this long after their
	// creation: they are deleted, media included. Zero keeps them.
	DraftTTL time.Duration `json:"draft_ttl,omitempty" yaml:"draft_ttl"`
	// Download is what a version's download serves; nil serves its
	// presentation's default (Download.Serves).
	Download *Download `json:"download,omitempty" yaml:"download"`
}

// Download is what a kind's version download serves. Whether a viewer may
// download, and which pages are marked, is the host's DownloadPolicy.
type Download struct {
	// Off: the kind's versions have no download.
	Off bool `json:"off,omitempty" yaml:"off"`
	// Serve is DownloadPages, DownloadOriginal or a private MP4 preset of the
	// kind's media ("mp4-1080"); empty is the presentation's default.
	Serve string `json:"serve,omitempty" yaml:"serve"`
}

// What a download serves.
const (
	// DownloadPages is a zip of the version's pages (the media kind's
	// Publish.Download), the default of readers.
	DownloadPages = "pages"
	// DownloadOriginal is the version's video as uploaded (the upload its HLS
	// ladder encodes), the default of players.
	DownloadOriginal = "original"
)

// Serves is what k's download serves: its Serve, else its presentation's
// default; "" serves none.
func (k Kind) Serves() string {
	d := k.Download
	switch {
	case d != nil && d.Off:
		return ""
	case d != nil && d.Serve != "":
		return d.Serve
	case k.Presentation == "reader":
		return DownloadPages
	case k.Presentation == "player":
		return DownloadOriginal
	}
	return ""
}

func (d Download) validate() error {
	if d.Off && d.Serve != "" {
		return errors.New("download is off: it serves nothing")
	}
	if d.Serve != "" && d.Serve != DownloadPages && d.Serve != DownloadOriginal && !routeRE.MatchString(d.Serve) {
		return fmt.Errorf("download serves %q: pages, original or a preset name", d.Serve)
	}
	return nil
}

// Locked is what a viewer who does not open content (or a version) still
// sees of it: the properties Show names and the free part Preview names. Its
// id, code, path, title, languages, traits, owner, creators and paywall
// always show; its body and private media never do.
type Locked struct {
	Show []string `json:"show" yaml:"show"`
	// Preview is the free part when its access rule names none; nil is none.
	Preview *Preview `json:"preview,omitempty" yaml:"preview"`
}

// Properties a Locked view may show.
const (
	ShowDescription = "description" // the content's and versions' descriptions
	ShowCover       = "cover"
	ShowGroups      = "entities"
	ShowAttributes  = "attributes" // the content's and versions' attributes
	ShowLength      = "length"     // versions' page counts, durations and segments
	ShowReactions   = "reactions"  // like and dislike counts; locked viewers may react
	ShowFavorites   = "favorites"  // favorite counts
	ShowViews       = "views"      // view counts
	ShowComments    = "comments"   // comments and their counts; locked viewers may comment
)

// Shown are the properties a Locked view may show.
var Shown = []string{ShowDescription, ShowCover, ShowGroups, ShowAttributes, ShowLength, ShowReactions, ShowFavorites, ShowViews, ShowComments}

// DefaultLocked shows a kind's metadata and cover: no preview, counts or
// comments.
var DefaultLocked = Locked{Show: []string{ShowDescription, ShowCover, ShowGroups, ShowAttributes, ShowLength}}

// LockedView is k's Locked, else DefaultLocked.
func (k Kind) LockedView() Locked {
	if k.Locked != nil {
		return *k.Locked
	}
	return DefaultLocked
}

// Shows reports whether l shows property p.
func (l Locked) Shows(p string) bool { return slices.Contains(l.Show, p) }

func (l Locked) validate(segments bool) error {
	for i, p := range l.Show {
		if !slices.Contains(Shown, p) || slices.Contains(l.Show[:i], p) {
			return fmt.Errorf("locked shows unknown or repeated %q (one of %v)", p, Shown)
		}
	}
	if p := l.Preview; p != nil {
		if err := p.Validate(); err != nil {
			return err
		}
		if p.Segments > 0 && !segments {
			return errors.New("locked previews segments but the kind has none")
		}
	}
	return nil
}

// Preview is the free part of locked content: its first Pages pages or first
// Segments segments (chapters).
type Preview struct {
	Pages    int `json:"pages,omitempty" yaml:"pages"`
	Segments int `json:"segments,omitempty" yaml:"segments"`
}

// Validate refuses a preview of nothing or of negative parts.
func (p Preview) Validate() error {
	if p.Pages < 0 || p.Segments < 0 || p.Pages == 0 && p.Segments == 0 {
		return errors.New("preview needs positive pages or segments")
	}
	return nil
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
	// Uploads limit what editors other than staff upload to a user-owned
	// group's media.
	Uploads UploadLimits `json:"uploads,omitzero" yaml:"uploads"`
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

// UploadLimits bound a user-owned group's uploads; zero fields are off.
type UploadLimits struct {
	// FilesPerHour and BytesPerDay limit each uploader.
	FilesPerHour int   `json:"files_per_hour,omitempty" yaml:"files_per_hour"`
	BytesPerDay  int64 `json:"bytes_per_day,omitempty" yaml:"bytes_per_day"`
	// QuotaBytes caps the group's stored originals.
	QuotaBytes int64 `json:"quota_bytes,omitempty" yaml:"quota_bytes"`
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
		if u := gk.Uploads; u.FilesPerHour < 0 || u.BytesPerDay < 0 || u.QuotaBytes < 0 || u != (UploadLimits{}) && gk.Owner != "user" {
			return fmt.Errorf("%w: group kind %s: uploads are non-negative limits of user-owned groups", ErrInvalid, name)
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
		if k.DraftTTL < 0 {
			return fmt.Errorf("%w: kind %s draft_ttl is negative", ErrInvalid, name)
		}
		if l := k.Locked; l != nil {
			if err := l.validate(k.Segments); err != nil {
				return fmt.Errorf("%w: kind %s: %v", ErrInvalid, name, err)
			}
		}
		if d := k.Download; d != nil {
			if err := d.validate(); err != nil {
				return fmt.Errorf("%w: kind %s: %v", ErrInvalid, name, err)
			}
		}
	}
	for _, r := range c.groupRefs() {
		if _, ok := c.GroupKinds[r.Kind]; !ok || r.Slug == "" {
			return fmt.Errorf("%w: group %s/%s names an unknown group kind or no slug", ErrInvalid, r.Kind, r.Slug)
		}
	}
	filters := map[GroupRef]bool{}
	for _, f := range c.ContentFilters {
		if filters[f.Group] {
			return fmt.Errorf("%w: content filter %s/%s is repeated", ErrInvalid, f.Group.Kind, f.Group.Slug)
		}
		filters[f.Group] = true
	}
	sites := map[string]bool{}
	for i, s := range c.Sites {
		if s.Name == "" || (len(s.Hosts) == 0 && i > 0) {
			return fmt.Errorf("%w: site %d needs a name and hosts", ErrInvalid, i)
		}
		if sites[s.Name] {
			return fmt.Errorf("%w: site name %q is repeated", ErrInvalid, s.Name)
		}
		sites[s.Name] = true
		defaults := map[GroupRef]bool{}
		for _, d := range s.ContentFilterDefaults {
			if !filters[d.Group] {
				return fmt.Errorf("%w: site %s default names undeclared content filter %s/%s", ErrInvalid, s.Name, d.Group.Kind, d.Group.Slug)
			}
			if defaults[d.Group] {
				return fmt.Errorf("%w: site %s default repeats content filter %s/%s", ErrInvalid, s.Name, d.Group.Kind, d.Group.Slug)
			}
			defaults[d.Group] = true
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

// SiteFor returns the site serving host, ignoring case and port; the first
// site when none matches. Host is the request host, not an authority selector.
func (c Config) SiteFor(host string) (Site, bool) {
	host = siteHost(host)
	for _, s := range c.Sites {
		for _, h := range s.Hosts {
			if siteHost(h) == host {
				return s, true
			}
		}
	}
	if len(c.Sites) > 0 {
		return c.Sites[0], true
	}
	return Site{}, false
}

func siteHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}
