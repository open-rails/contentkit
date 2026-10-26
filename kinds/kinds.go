// Package kinds is the site's content model as configuration: content kinds,
// entity kinds, tiers, restrictions and domain visibility policies. One
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

// Config is the application's content model.
type Config struct {
	Languages      []string              `json:"languages" yaml:"languages"`
	Tiers          []string              `json:"tiers,omitempty" yaml:"tiers"`
	TokenClaims    []string              `json:"token_claims,omitempty" yaml:"token_claims"`
	Restrictions   []string              `json:"restrictions,omitempty" yaml:"restrictions"`
	ContentFilters ContentFilters        `json:"content_filters,omitempty" yaml:"content_filters"`
	Kinds          map[string]Kind       `json:"content_kinds" yaml:"content_kinds"`
	EntityKinds    map[string]EntityKind `json:"entity_kinds,omitempty" yaml:"entity_kinds"`
	Channels       *Channels             `json:"channels,omitempty" yaml:"channels"`
	channelStorage bool
}

// ContentFilters is one app's shared entity-filter policy. Filters apply to
// every selected content kind; required entities are independently scoped.
type ContentFilters struct {
	AppliesTo []string `json:"applies_to" yaml:"applies_to"`
	Domains   []Domain `json:"domains" yaml:"domains"`
}

// Clone gives an embedded engine its own immutable policy snapshot, including
// the optional bool pointers and the per-kind required-entity slices.
func (c ContentFilters) Clone() ContentFilters {
	out := ContentFilters{AppliesTo: slices.Clone(c.AppliesTo), Domains: slices.Clone(c.Domains)}
	for i, domain := range out.Domains {
		domain.Hosts = slices.Clone(domain.Hosts)
		if domain.RequireEntities != nil {
			domain.RequireEntities = make(map[string][]EntityRef, len(domain.RequireEntities))
			for kind, refs := range c.Domains[i].RequireEntities {
				domain.RequireEntities[kind] = slices.Clone(refs)
			}
		}
		domain.Filters = slices.Clone(domain.Filters)
		for j, filter := range domain.Filters {
			if filter.UserToggle != nil {
				value := *filter.UserToggle
				domain.Filters[j].UserToggle = &value
			}
		}
		out.Domains[i] = domain
	}
	return out
}

// Domain is an entrypoint matched by request host. The domain named main is
// the fallback for unmatched hosts. Name is its stable branding/activity identity;
// entity display names belong to entity records, never policy config.
type Domain struct {
	Name            string                 `json:"name" yaml:"name"`
	Hosts           []string               `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	RequireEntities map[string][]EntityRef `json:"require_entities,omitempty" yaml:"require_entities,omitempty"`
	Filters         []ContentFilter        `json:"filters,omitempty" yaml:"filters,omitempty"`
}

// Requires reports whether any content kind requires this entity. Required
// entities never appear as optional controls that could empty that selection.
func (d Domain) Requires(ref EntityRef) bool {
	for _, refs := range d.RequireEntities {
		if slices.Contains(refs, ref) {
			return true
		}
	}
	return false
}

// EntityRef names an entity by kind and canonical slug.
type EntityRef struct {
	Kind string `json:"kind" yaml:"kind"`
	Slug string `json:"slug" yaml:"slug"`
}

// ContentFilter defines this domain's visibility for one entity. A nil
// UserToggle allows reader overrides. Internal and required entities are
// never reader-toggleable, irrespective of UserToggle.
type ContentFilter struct {
	Entity     EntityRef `json:"-" yaml:",inline"`
	Visible    bool      `json:"visible" yaml:"visible"`
	UserToggle *bool     `json:"user_toggle,omitempty" yaml:"user_toggle,omitempty"`
}

// Toggleable reports whether this rule permits reader overrides. The caller
// must additionally exclude internal and domain-required entities.
func (f ContentFilter) Toggleable() bool { return f.UserToggle == nil || *f.UserToggle }

// Kind is one content kind: a gallery, a movie, a creator post, a blog post.
// TitlePolicy chooses how a kind's display title is resolved.
type TitlePolicy string

const (
	TitleLocalized          TitlePolicy = "localized"
	TitleOriginalAndVersion TitlePolicy = "original_and_version"
)

// AccessScope chooses where the editor presents the access rule. Both scopes
// retain the same permission checks and optional per-version overrides.
type AccessScope string

const (
	AccessContent AccessScope = "content"
	AccessVersion AccessScope = "version"
)

type Kind struct {
	// TitlePolicy defaults to localized text. OriginalAndVersion uses the
	// selected edition title, then the shared original title, then legacy text.
	TitlePolicy TitlePolicy `json:"title_policy,omitempty" yaml:"title_policy"`

	// AccessLevels limits content and version rules. Empty permits every level.
	// Omitting ppv and group_ppv removes individual-purchase access modes.
	AccessLevels []string `json:"access_levels,omitempty" yaml:"access_levels"`
	// AccessScope defaults to content; version presents a direct edition rule.
	AccessScope AccessScope `json:"access_scope,omitempty" yaml:"access_scope"`
	// CommentModes limits per-item comment settings. Empty permits every mode.
	CommentModes []string `json:"comment_modes,omitempty" yaml:"comment_modes"`
	// Route is the URL segment: /{lang}/{route}/{CODE}/{slug}.
	Route string `json:"route" yaml:"route"`
	// PathEntity names one of Entities whose slug prefixes the title in the
	// decorative path, /{lang}/{route}/{CODE}/{entity}/{title}, for SEO.
	PathEntity string `json:"path_entity,omitempty" yaml:"path_entity"`
	// Presentation tells contentkit-ui how to show it: reader, player or post.
	Presentation string       `json:"presentation" yaml:"presentation"`
	Versions     VersionRules `json:"versions" yaml:"versions"`
	// Segments enables ordered, individually released ranges (chapters).
	Segments bool `json:"segments,omitempty" yaml:"segments"`
	// Entities are the entity kinds content of this kind may join.
	Entities   []string             `json:"entities,omitempty" yaml:"entities"`
	Attributes map[string]Attribute `json:"attributes,omitempty" yaml:"attributes"`
	// Channel declares the one channel governing the overall item and all versions.
	Channel *ChannelPolicy `json:"channel,omitempty" yaml:"channel"`
	// Sort is the default list order.
	Sort string `json:"sort,omitempty" yaml:"sort"`
	// Locked is what a viewer who does not open its content sees of it;
	// nil is DefaultLocked.
	Locked *Locked `json:"locked,omitempty" yaml:"locked"`
	// Download is what a version's download serves; nil serves its
	// presentation's default (Download.Serves).
	Download *Download `json:"download,omitempty" yaml:"download"`
}

// AllowsAccess reports whether this kind accepts a rule's access level.
func (k Kind) AllowsAccess(level string) bool {
	return len(k.AccessLevels) == 0 || slices.Contains(k.AccessLevels, level)
}

// AllowsComments reports whether this kind accepts a per-item comment mode.
func (k Kind) AllowsComments(mode string) bool {
	return len(k.CommentModes) == 0 || slices.Contains(k.CommentModes, mode)
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
	ShowEntities    = "entities"
	ShowAttributes  = "attributes" // the content's and versions' attributes
	ShowLength      = "length"     // versions' page counts, durations and segments
	ShowReactions   = "reactions"  // like and dislike counts; locked viewers may react
	ShowFavorites   = "favorites"  // favorite counts
	ShowComments    = "comments"   // comments and their counts; locked viewers may comment
)

// Shown are the properties a Locked view may show.
var Shown = []string{ShowDescription, ShowCover, ShowEntities, ShowAttributes, ShowLength, ShowReactions, ShowFavorites, ShowComments}

// DefaultLocked shows a kind's metadata and cover: no preview, counts or
// comments.
var DefaultLocked = Locked{Show: []string{ShowDescription, ShowCover, ShowEntities, ShowAttributes, ShowLength}}

// LockedView is k's Locked, else DefaultLocked.
func (k Kind) LockedView() Locked {
	if k.Locked != nil {
		return *k.Locked
	}
	return DefaultLocked
}

// Shows reports whether l shows property p.
func (l Locked) Shows(p string) bool { return slices.Contains(l.Show, p) }

func (l Locked) validate() error {
	for i, p := range l.Show {
		if !slices.Contains(Shown, p) || slices.Contains(l.Show[:i], p) {
			return fmt.Errorf("locked shows unknown or repeated %q (one of %v)", p, Shown)
		}
	}
	if p := l.Preview; p != nil {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Preview selects public preview renditions of locked content's first Pages.
// Chapters share the content item's access rule and cannot be free subsets.
type Preview struct {
	Pages    int `json:"pages,omitempty" yaml:"pages"`
	Segments int `json:"segments,omitempty" yaml:"segments"` // legacy input; nonzero values are refused
}

// Validate refuses empty previews and legacy chapter-subset access.
func (p Preview) Validate() error {
	if p.Segments != 0 {
		return errors.New("preview segments are unsupported: chapters share item access")
	}
	if p.Pages <= 0 {
		return errors.New("preview needs positive pages")
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
	// Required fields must be present on completed content metadata. Empty
	// drafts may omit them while media uploads; versions inherit the parent.
	Required bool `json:"required,omitempty" yaml:"required"`
}

// MembershipScope identifies which record owns an entity association.
type MembershipScope string

const (
	ContentScope MembershipScope = "content"
	VersionScope MembershipScope = "version"
)

// EntityKind is one kind of grouping: tag, artist, series, channel, rail, ...
type EntityKind struct {
	Route string `json:"route,omitempty" yaml:"route"`
	// Ordered members carry positions (series, playlists, rails).
	Ordered bool `json:"ordered,omitempty" yaml:"ordered"`
	// Parent is the entity kind an entity may sit under (series under series).
	Parent string `json:"parent,omitempty" yaml:"parent"`
	// ScopedBy makes slugs unique within an entity of that kind (character
	// within series).
	ScopedBy string `json:"scoped_by,omitempty" yaml:"scoped_by"`
	// Owner is "site" (staff-managed) or "user" (channels, playlists): a
	// signed-in person creates a user-owned entity and owns it.
	Owner string `json:"-" yaml:"-"`
	// MaxPerOwner caps the active entities of the kind one person owns; 0 is
	// no cap. Staff are not capped.
	MaxPerOwner int `json:"-" yaml:"-"`
	// Avatar gives site-owned entities a square avatar beside their cover;
	// user-owned kinds always have one.
	Avatar bool `json:"avatar,omitempty" yaml:"avatar"`
	// Uploads limit what editors other than staff upload to a user-owned
	// entity's media.
	Uploads UploadLimits `json:"-" yaml:"-"`
	// Entitled entities expose membership backed by their stable entitlement key.
	Entitled bool `json:"-" yaml:"-"`
	// AuthScope entities name an AuthKit group whose permissions govern their
	// content (OnlyDemo channels).
	AuthScope bool `json:"-" yaml:"-"`
	// MembershipScopes declares where this entity kind can be attached.
	// Omitted keeps the legacy default: content, or version with VersionLevel.
	MembershipScopes []MembershipScope `json:"membership_scopes,omitempty" yaml:"membership_scopes"`
	// VersionLevel is the legacy version-only editor setting. Prefer
	// MembershipScopes; it cannot be combined with that explicit setting.
	VersionLevel bool `json:"version_level,omitempty" yaml:"version_level"`
	// Creatable lets a content edit create an entity of the kind from a name no
	// entity carries yet; never user-owned or authorization kinds.
	Creatable bool `json:"creatable,omitempty" yaml:"creatable"`
	// Roles are membership roles (publisher on an artist entity).
	Roles      []string             `json:"roles,omitempty" yaml:"roles"`
	Attributes map[string]Attribute `json:"attributes,omitempty" yaml:"attributes"`
}

// HasAvatar reports whether the kind's entities take an avatar.
func (k EntityKind) HasAvatar() bool { return k.Owner == "user" || k.Avatar }

// AllowsMembership reports the configured ownership scopes, including the
// legacy editor default when no explicit scopes were supplied.
func (k EntityKind) AllowsMembership(scope MembershipScope) bool {
	if len(k.MembershipScopes) > 0 {
		return slices.Contains(k.MembershipScopes, scope)
	}
	if k.VersionLevel {
		return scope == VersionScope
	}
	return scope == ContentScope
}

// UploadLimits bound a user-owned entity's uploads; zero fields are off.
type UploadLimits struct {
	// FilesPerHour and BytesPerDay limit each uploader.
	FilesPerHour int   `json:"files_per_hour,omitempty" yaml:"files_per_hour"`
	BytesPerDay  int64 `json:"bytes_per_day,omitempty" yaml:"bytes_per_day"`
	// QuotaBytes caps the entity's stored originals.
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
	for name, gk := range c.EntityKinds {
		if name != "channel" && (gk.Owner == "user" || gk.AuthScope || gk.Entitled || gk.MaxPerOwner != 0 || gk.Uploads != (UploadLimits{})) {
			return fmt.Errorf("%w: authoring ownership, scopes and paid membership belong to channels, not metadata kind %s", ErrInvalid, name)
		}
		if name == "channel" && !c.channelStorage {
			return fmt.Errorf("%w: channel is configured through channels, not entity_kinds", ErrInvalid)
		}
		if !identRE.MatchString(name) {
			return fmt.Errorf("%w: group kind %q", ErrInvalid, name)
		}
		if _, clash := c.Kinds[name]; clash {
			return fmt.Errorf("%w: %q is both a kind and a group kind", ErrInvalid, name)
		}
		if err := route("group kind "+name, gk.Route); err != nil {
			return err
		}
		if gk.Creatable && (gk.Owner == "user" || gk.AuthScope) {
			return fmt.Errorf("%w: group kind %q: user-owned and auth_scope kinds are not creatable", ErrInvalid, name)
		}
		if len(gk.MembershipScopes) > 0 && gk.VersionLevel {
			return fmt.Errorf("%w: group kind %s combines membership_scopes with legacy version_level", ErrInvalid, name)
		}
		seenScopes := map[MembershipScope]bool{}
		for _, scope := range gk.MembershipScopes {
			if (scope != ContentScope && scope != VersionScope) || seenScopes[scope] {
				return fmt.Errorf("%w: group kind %s has invalid or repeated membership scope %q", ErrInvalid, name, scope)
			}
			seenScopes[scope] = true
		}
		for _, ref := range []string{gk.Parent, gk.ScopedBy} {
			if ref == "channel" {
				return fmt.Errorf("%w: metadata does not parent or scope channels", ErrInvalid)
			}
			if _, ok := c.EntityKinds[ref]; ref != "" && !ok {
				return fmt.Errorf("%w: group kind %s names unknown group kind %q", ErrInvalid, name, ref)
			}
		}
		if gk.Owner != "" && gk.Owner != "site" && gk.Owner != "user" {
			return fmt.Errorf("%w: group kind %s owner %q", ErrInvalid, name, gk.Owner)
		}
		if gk.MaxPerOwner < 0 || gk.MaxPerOwner > 0 && gk.Owner != "user" {
			return fmt.Errorf("%w: group kind %s: max_per_owner is a non-negative cap of user-owned groups", ErrInvalid, name)
		}
		if u := gk.Uploads; u.FilesPerHour < 0 || u.BytesPerDay < 0 || u.QuotaBytes < 0 || u != (UploadLimits{}) && gk.Owner != "user" && name != "channel" {
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
	if ch := c.Channels; ch != nil {
		if ch.Creation != "staff" && ch.Creation != "users" {
			return fmt.Errorf("%w: channels.creation must be staff or users", ErrInvalid)
		}
		if !c.channelStorage {
			if err := route("channels", ch.Route); err != nil {
				return err
			}
		}
		if ch.MaxPerOwner < 0 || ch.MaxPerOwner > 0 && ch.Creation != "users" {
			return fmt.Errorf("%w: channels.max_per_owner requires users creation", ErrInvalid)
		}
		if u := ch.Uploads; u.FilesPerHour < 0 || u.BytesPerDay < 0 || u.QuotaBytes < 0 {
			return fmt.Errorf("%w: channel upload limits must be nonnegative", ErrInvalid)
		}
		for _, name := range ch.Entities {
			if _, ok := c.EntityKinds[name]; !ok || name == "channel" {
				return fmt.Errorf("%w: unknown channel metadata kind %q", ErrInvalid, name)
			}
		}
	}
	for name, k := range c.Kinds {
		if name == "channel" {
			return fmt.Errorf("%w: channel is reserved for authoring channels", ErrInvalid)
		}
		if k.AccessScope != "" && k.AccessScope != AccessContent && k.AccessScope != AccessVersion {
			return fmt.Errorf("%w: kind %s has invalid access_scope %q", ErrInvalid, name, k.AccessScope)
		}
		if k.AccessScope == AccessVersion && k.Versions.Hidden {
			return fmt.Errorf("%w: kind %s cannot edit access on a hidden version", ErrInvalid, name)
		}
		if k.TitlePolicy != "" && k.TitlePolicy != TitleLocalized && k.TitlePolicy != TitleOriginalAndVersion {
			return fmt.Errorf("%w: kind %s has invalid title_policy %q", ErrInvalid, name, k.TitlePolicy)
		}
		seenComments := map[string]bool{}
		for _, mode := range k.CommentModes {
			if !slices.Contains([]string{"open", "closed", "hidden"}, mode) || seenComments[mode] {
				return fmt.Errorf("%w: kind %s has invalid or duplicate comment mode %q", ErrInvalid, name, mode)
			}
			seenComments[mode] = true
		}
		seenLevels := map[string]bool{}
		for _, level := range k.AccessLevels {
			if !slices.Contains([]string{"public", "tier", "ppv", "group", "group_ppv"}, level) || seenLevels[level] {
				return fmt.Errorf("%w: kind %s has invalid or duplicate access level %q", ErrInvalid, name, level)
			}
			if level == "tier" && len(c.Tiers) == 0 {
				return fmt.Errorf("%w: kind %s allows tier access without any tiers", ErrInvalid, name)
			}
			seenLevels[level] = true
		}
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
		for _, g := range k.Entities {
			if _, ok := c.EntityKinds[g]; !ok || g == "channel" {
				return fmt.Errorf("%w: kind %s names unknown group kind %q", ErrInvalid, name, g)
			}
		}
		if policy := k.Channel; policy != nil {
			if c.Channels == nil {
				return fmt.Errorf("%w: kind %s uses channels without channels configuration", ErrInvalid, name)
			}
			if policy.Fixed != "" && (strings.TrimSpace(policy.Fixed) != policy.Fixed || !routeRE.MatchString(policy.Fixed)) {
				return fmt.Errorf("%w: kind %s has invalid fixed channel slug", ErrInvalid, name)
			}
			if policy.Fixed != "" && c.Channels.Creation == "users" {
				return fmt.Errorf("%w: fixed channels require staff creation", ErrInvalid)
			}
			if k.AccessScope == AccessVersion {
				return fmt.Errorf("%w: channel content uses parent-level access", ErrInvalid)
			}
		}
		if e := k.PathEntity; e != "" && (!slices.Contains(k.Entities, e) || !c.EntityKinds[e].AllowsMembership(ContentScope)) {
			return fmt.Errorf("%w: kind %s path_entity %q must be one of its content-level groups", ErrInvalid, name, e)
		}
		if err := validateAttributes("kind "+name, k.Attributes); err != nil {
			return err
		}
		if l := k.Locked; l != nil {
			if err := l.validate(); err != nil {
				return fmt.Errorf("%w: kind %s: %v", ErrInvalid, name, err)
			}
		}
		if d := k.Download; d != nil {
			if err := d.validate(); err != nil {
				return fmt.Errorf("%w: kind %s: %v", ErrInvalid, name, err)
			}
		}
	}
	for _, r := range c.entityRefs() {
		if _, ok := c.EntityKinds[r.Kind]; !ok || r.Slug == "" {
			return fmt.Errorf("%w: group %s/%s names an unknown group kind or no slug", ErrInvalid, r.Kind, r.Slug)
		}
	}
	if err := c.ContentFilters.validateDomains(); err != nil {
		return err
	}
	applies := map[string]bool{}
	for _, kind := range c.ContentFilters.AppliesTo {
		if _, ok := c.Kinds[kind]; !ok || applies[kind] {
			return fmt.Errorf("%w: applies_to names an unknown or repeated content kind %q", ErrInvalid, kind)
		}
		applies[kind] = true
	}
	for _, d := range c.ContentFilters.Domains {
		if len(d.Filters) > 0 && len(applies) == 0 {
			return fmt.Errorf("%w: domain %s filters require explicit applies_to content kinds", ErrInvalid, d.Name)
		}
		filters := map[EntityRef]bool{}
		for _, f := range d.Filters {
			if filters[f.Entity] {
				return fmt.Errorf("%w: domain %s repeats filter %s/%s", ErrInvalid, d.Name, f.Entity.Kind, f.Entity.Slug)
			}
			filters[f.Entity] = true
			if d.Requires(f.Entity) && !f.Visible {
				return fmt.Errorf("%w: domain %s both requires and hides entity %s/%s", ErrInvalid, d.Name, f.Entity.Kind, f.Entity.Slug)
			}
			for kind := range applies {
				if !slices.Contains(c.Kinds[kind].Entities, f.Entity.Kind) {
					return fmt.Errorf("%w: domain %s filter entity kind %s cannot be associated with %s", ErrInvalid, d.Name, f.Entity.Kind, kind)
				}
			}
		}
		for kind, refs := range d.RequireEntities {
			model, ok := c.Kinds[kind]
			if !ok {
				return fmt.Errorf("%w: domain %s names unknown required content kind %q", ErrInvalid, d.Name, kind)
			}
			seen := map[EntityRef]bool{}
			for _, r := range refs {
				if seen[r] || !slices.Contains(model.Entities, r.Kind) {
					return fmt.Errorf("%w: domain %s has repeated or unsupported required entity %s/%s for %s", ErrInvalid, d.Name, r.Kind, r.Slug, kind)
				}
				seen[r] = true
			}
		}
	}
	return nil
}

func (c Config) entityRefs() []EntityRef {
	var out []EntityRef
	for _, d := range c.ContentFilters.Domains {
		for _, refs := range d.RequireEntities {
			out = append(out, refs...)
		}
		for _, f := range d.Filters {
			out = append(out, f.Entity)
		}
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

// ValidateRequiredAttributes checks a content's complete attributes after
// normalization. This is separate from ValidateAttributes because version
// attributes are overrides, and newly allocated drafts can be incomplete.
func ValidateRequiredAttributes(declared map[string]Attribute, values map[string]any) error {
	for name, spec := range declared {
		if !spec.Required {
			continue
		}
		value, present := values[name]
		if !present || value == nil {
			return fmt.Errorf("%w: attribute %s is required", ErrInvalid, name)
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			return fmt.Errorf("%w: attribute %s is required", ErrInvalid, name)
		}
	}
	return nil
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

// Routes maps every content kind and entity kind with a route to it: the
// first path segment of its pages (contenturl.Routes).
func (c Config) Routes() map[string]string {
	out := map[string]string{}
	if c.Channels != nil && c.Channels.Route != "" {
		out["channel"] = c.Channels.Route
	}
	for name, k := range c.Kinds {
		out[name] = k.Route
	}
	for name, gk := range c.EntityKinds {
		if gk.Route != "" {
			out[name] = gk.Route
		}
	}
	return out
}

func (c ContentFilters) validateDomains() error {
	if len(c.Domains) == 0 {
		return nil
	}
	mainDomains := 0
	for _, domain := range c.Domains {
		if domain.Name == "main" {
			mainDomains++
		}
	}
	if mainDomains != 1 {
		return fmt.Errorf("%w: configured domains need exactly one domain named main", ErrInvalid)
	}
	names, hosts := map[string]bool{}, map[string]bool{}
	for _, domain := range c.Domains {
		if domain.Name == "" || names[domain.Name] || (domain.Name != "main" && len(domain.Hosts) == 0) {
			return fmt.Errorf("%w: domain %q needs a unique name and non-main domains need hosts", ErrInvalid, domain.Name)
		}
		names[domain.Name] = true
		for _, alias := range domain.Hosts {
			host := domainHost(alias)
			if host == "" || hosts[host] {
				return fmt.Errorf("%w: domain host %q is empty or repeated", ErrInvalid, alias)
			}
			hosts[host] = true
		}
	}
	return nil
}

// DomainFor returns an exact hostname match, ignoring case, port and the DNS
// trailing dot, or the domain named main. Domain order has no meaning. Host is
// the request host, not an authority selector; no configured policy returns false.
func (c Config) DomainFor(host string) (Domain, bool) {
	host = domainHost(host)
	if host != "" {
		for _, domain := range c.ContentFilters.Domains {
			for _, alias := range domain.Hosts {
				if domainHost(alias) == host {
					return domain, true
				}
			}
		}
	}
	for _, domain := range c.ContentFilters.Domains {
		if domain.Name == "main" {
			return domain, true
		}
	}
	return Domain{}, false
}

func domainHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
}
