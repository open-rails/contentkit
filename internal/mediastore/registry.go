package mediastore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/endpoint"
	"github.com/open-rails/contentkit/internal/uuidv7"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

var ErrUnknownKind = errors.New("media: unknown kind")

// ErrInvalidAccountID is an Account kind's id that is not a canonical UUID.
var ErrInvalidAccountID = errors.New("media: an account id must be a canonical lowercase UUID")

// maxWidth bounds a public width.
const maxWidth = 8192

// Registry is a validated Config with the app's Hooks.
type Registry struct {
	cfg      media.Config
	hooks    Hooks
	declared int // cfg.Kinds[:declared] are c's
	kinds    map[string]*RegisteredKind
	all      []*RegisteredKind // declared storage kinds
	// defaults reads a kind's default images (Manifests sets it).
	defaults func(ctx context.Context, k *RegisteredKind, p *media.Public) (media.PublicImage, bool)
	// shown memoizes defaults for defaultTTL: they change only when
	// publish-defaults runs.
	shown sync.Map // kind/preset -> shownDefault
}

const defaultTTL = time.Minute

type shownDefault struct {
	image media.PublicImage
	ok    bool
	until time.Time
}

// defaultImage is preset p's default image of k, memoized.
func (r *Registry) defaultImage(ctx context.Context, k *RegisteredKind, p *media.Public) (media.PublicImage, bool) {
	if r.defaults == nil || !hasDefault(p) {
		return media.PublicImage{}, false
	}
	key := k.Name + "/" + p.Name
	if v, ok := r.shown.Load(key); ok && time.Now().Before(v.(shownDefault).until) {
		return v.(shownDefault).image, v.(shownDefault).ok
	}
	image, ok := r.defaults(ctx, k, p)
	if ctx.Err() == nil {
		r.shown.Store(key, shownDefault{image: image, ok: ok, until: time.Now().Add(defaultTTL)})
	}
	return image, ok
}

// NewRegistry validates c and its version reader configuration.
func NewRegistry(c media.Config, hooks Hooks) (*Registry, error) {
	if !c.Scope.IsZero() {
		if !c.Scope.Valid() || c.Scope.AppID == "" || c.Namespace != "" && c.Namespace != c.Scope.Partition() {
			return nil, errors.New("media: Scope requires tenant and app IDs and no legacy Namespace")
		}
		c.Namespace = c.Scope.Partition()
	}
	if !layout.ValidSegment(c.Namespace) {
		return nil, fmt.Errorf("media: invalid namespace %q", c.Namespace)
	}
	if c.BaseURL != "" {
		u, err := endpoint.Secure(c.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("media: BaseURL: %w", err)
		}
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("media: BaseURL %q must be an origin", c.BaseURL)
		}
	}
	if c.Editor == (media.Image{}) {
		c.Editor = media.DefaultEditor
	}
	if err := validateImage(c.Editor); err != nil {
		return nil, fmt.Errorf("media: Editor: %w", err)
	}
	if c.Editor.Format != media.ImageFormatWebP {
		return nil, errors.New("media: Editor images use WebP")
	}
	kinds := slices.Clone(c.Kinds)
	r := &Registry{cfg: c, hooks: hooks, declared: len(c.Kinds), kinds: make(map[string]*RegisteredKind, len(kinds))}
	r.cfg.Kinds = make([]media.Kind, len(kinds))
	for i, decl := range kinds {
		k := &RegisteredKind{Kind: decl}
		if _, dup := r.kinds[k.Name]; dup {
			return nil, fmt.Errorf("media: duplicate kind %q", k.Name)
		}
		if k.Namespace != "" && (!layout.ValidSegment(k.Namespace) || k.Namespace == c.Namespace) {
			return nil, fmt.Errorf("media: kind %q: invalid shared namespace %q (an app's namespace is never a shared one)", k.Name, k.Namespace)
		}
		k.Uploads, k.Private, k.Public = slices.Clone(k.Uploads), slices.Clone(k.Private), slices.Clone(k.Public)
		if err := k.validate(); err != nil {
			return nil, fmt.Errorf("media: kind %q: %w", k.Name, err)
		}
		if err := k.Master.Validate(); err != nil {
			return nil, fmt.Errorf("media: kind %q: %w", k.Name, err)
		}
		k.ns = cmpOr(k.Namespace, c.Namespace)
		if !c.Scope.IsZero() {
			if k.Namespace != "" {
				return nil, fmt.Errorf("media: scoped kind %q cannot choose a legacy Namespace", k.Name)
			}
			k.scope = c.Scope
			if k.Account {
				k.scope.AppID = ""
			}
			k.ns = k.scope.Partition()
		} else if k.Account {
			return nil, fmt.Errorf("media: account kind %q requires Scope", k.Name)
		}
		if err := k.levels(); err != nil {
			return nil, fmt.Errorf("media: kind %q: %w", k.Name, err)
		}
		r.cfg.Kinds[i] = k.Kind
		r.kinds[k.Name] = k
		r.all = append(r.all, k)
	}
	if a := c.Avatars; a != (media.Avatars{}) {
		k, ok := r.kinds[a.Kind]
		if !ok || k.public(a.Preset) == nil {
			return nil, fmt.Errorf("media: Avatars: kind %q has no public preset %q", a.Kind, a.Preset)
		}
	}
	return r, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Config is the registry's configuration, normalized.
func (r *Registry) Config() media.Config { return r.cfg }

// Hooks are the app's callbacks the registry was built with.
func (r *Registry) Hooks() Hooks { return r.hooks }

// Namespace is the app's own namespace.
func (r *Registry) Namespace() string { return r.cfg.Namespace }

// Kind returns a registered kind.
func (r *Registry) Kind(name string) (*RegisteredKind, error) {
	k, ok := r.kinds[name]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKind, name)
	}
	return k, nil
}

// Kinds are the declared storage kinds; reader chapters create no kinds.
func (r *Registry) Kinds() []*RegisteredKind { return slices.Clone(r.all) }

// Namespaces are the namespaces the app's items live in: its own, then the
// shared ones it imports.
func (r *Registry) Namespaces() []string {
	out := []string{r.cfg.Namespace}
	for _, k := range r.all {
		if !slices.Contains(out, k.ns) {
			out = append(out, k.ns)
		}
	}
	return out
}

// Ref is the ref of item id of kind, in the kind's namespace: a kind without
// versions.
func (r *Registry) Ref(kind, id string) (contentref.ContentRef, error) {
	return r.VersionRef(kind, id, "")
}

// VersionRef is the ref of version of a Versions kind's content id, or ("")
// Ref.
func (r *Registry) VersionRef(kind, id, version string) (contentref.ContentRef, error) {
	k, err := r.Kind(kind)
	if err != nil {
		return contentref.ContentRef{}, err
	}
	ref := contentref.New(k.ns, kind, id).WithVersion(version)
	_, err = r.Item(ref)
	return ref, err
}

// Item is a validated item: a ref of a registered kind in its namespace,
// and its folder's keys.
type Item struct {
	ref    contentref.ContentRef
	kind   *RegisteredKind
	prefix string
}

// Item validates ref: a registered kind, its namespace and a UUIDv7 id, or
// for an Account kind any canonical UUID (accounts keep their auth subject,
// such as an imported account's fixed UUIDv5). A Versions kind's ref names a
// version (a UUIDv7): a content has no item of its own; any other kind's has
// no version.
func (r *Registry) Item(ref contentref.ContentRef) (Item, error) {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return Item{}, err
	}
	if ref.TenantID != k.ns {
		return Item{}, fmt.Errorf("media: invalid ref %s: kind %q lives in namespace %q", ref, k.Name, k.ns)
	}
	if k.Account {
		if !canonicalUUID(ref.ContentID) {
			return Item{}, fmt.Errorf("media: invalid ref %s: %w", ref, ErrInvalidAccountID)
		}
	} else if err := contentref.ValidateID(ref.ContentID); err != nil {
		return Item{}, fmt.Errorf("media: invalid ref %s: %w", ref, err)
	}
	version := ref.Version()
	switch {
	case !k.Versions && ref.ContentVersionID != nil:
		return Item{}, fmt.Errorf("media: invalid ref %s: kind %q has no versions", ref, k.Name)
	case k.Versions && version == "":
		return Item{}, fmt.Errorf("media: invalid ref %s: kind %q keeps its media in versions", ref, k.Name)
	case k.Versions:
		if err := contentref.ValidateID(version); err != nil {
			return Item{}, fmt.Errorf("media: invalid ref %s: version: %w", ref, err)
		}
	}
	return Item{ref: ref, kind: k, prefix: k.prefix(ref.ContentID, version)}, nil
}

func (i Item) Ref() contentref.ContentRef { return i.ref }

func (i Item) Kind() *RegisteredKind { return i.kind }

// Prefix is the physical owner folder. Versioned kinds use the version ID;
// the parent content ID remains only in Ref and the catalog.
func (i Item) Prefix() string { return i.prefix }

// at dates an id derived from this folder (uuidv7.Name): the first UUIDv7
// among ids, else the folder's own id, its version's or its content's. An
// account's id need not be a UUIDv7; then it is the Unix epoch.
func (i Item) at(ids ...string) time.Time {
	return uuidv7.At(append(ids, i.ref.Version(), i.ref.ContentID)...)
}

func (i Item) ManifestKey() string { return i.prefix + layout.ManifestName }

func (i Item) PrivatePrefix() string { return i.prefix + layout.AreaPrivate + "/" }

func (i Item) PublicPrefix() string { return i.prefix + layout.AreaPublic + "/" }

func (i Item) InternalPrefix() string { return i.prefix + layout.AreaInternal + "/" }

func (i Item) TempPrefix() string { return i.prefix + layout.AreaTmp + "/" }

// Blob is the key of a blob reference, "{area}/{sha256}".
func (i Item) Blob(ref string) (string, error) {
	area, name, ok := layout.SplitBlobRef(ref)
	if !ok {
		return "", fmt.Errorf("media: invalid blob reference %q", ref)
	}
	return i.prefix + area + "/" + name, nil
}

// BlobRef is the reference of key, a stored file in the folder.
func (i Item) BlobRef(key string) (string, bool) {
	k, ok := layout.Parse(key)
	if !ok || k.Folder() != i.prefix || !layout.ValidBlobRef(k.Area+"/"+k.Name) {
		return "", false
	}
	return k.Area + "/" + k.Name, true
}

// Staged is the key of a staged upload ("u-{uuid}"), in tmp/ until placed.
func (i Item) Staged(name string) (string, error) {
	if !layout.ValidStagedName(name) {
		return "", fmt.Errorf("media: invalid staged upload name %q", name)
	}
	return i.TempPrefix() + name, nil
}

// DefaultsItem is the item holding the kind's default images, with its
// own manifest (image.PublishDefaults).
func (k *RegisteredKind) DefaultsItem() Item {
	return Item{ref: contentref.New(k.ns, k.Name, layout.DefaultID), kind: k, prefix: k.prefix(layout.DefaultID, "")}
}

// NS is the namespace the kind's items live in.
func (k *RegisteredKind) NS() string { return k.ns }

// PublicURL is the URL of item ref's public file hash at the registry's BaseURL.
func (r *Registry) PublicURL(ref contentref.ContentRef, hash string) string {
	item, err := r.Item(ref)
	if err != nil {
		return ""
	}
	return r.publicURL(item, hash)
}

func (r *Registry) publicURL(item Item, hash string) string {
	return strings.TrimRight(r.cfg.BaseURL, "/") + layout.URLPrefix + item.Prefix() + layout.AreaPublic + "/" + hash
}

// PresetRules lists every kind's public presets, in registry order.
func (r *Registry) PresetRules() []media.PresetRule {
	out := []media.PresetRule{}
	for _, k := range r.all {
		for _, p := range k.Public {
			out = append(out, media.PresetRule{Kind: k.Name, Name: p.Name, From: p.From, Base: strings.TrimRight(r.cfg.BaseURL, "/"),
				Namespace: k.ns, Root: k.root(), To: p.To, Widths: append([]int{}, p.Widths...), Aspect: p.Image.Aspect, MinWidth: p.Image.MinWidth,
			})
		}
	}
	return out
}

// PresetRulesWithDefaults is PresetRules with each preset's default image
// read from its kind's defaults manifest.
func (r *Registry) PresetRulesWithDefaults(ctx context.Context) []media.PresetRule {
	out := r.PresetRules()
	for i := range out {
		k, err := r.Kind(out[i].Kind)
		if err != nil {
			continue
		}
		if p := k.public(out[i].Name); p != nil {
			if image, ok := r.defaultImage(ctx, k, p); ok {
				out[i].DefaultImage = &image
			}
		}
	}
	return out
}

// MarshalJSON is the registry as data (no hooks, defaults or derived kinds,
// which NewRegistry derives again): the stock worker's MEDIA_KINDS_FILE.
func (r *Registry) MarshalJSON() ([]byte, error) {
	cfg := r.cfg
	cfg.Kinds = cfg.Kinds[:r.declared]
	return json.Marshal(cfg)
}

// Scope identifies the registry's trusted app owner; false means legacy mode.
func (r *Registry) Scope() (layout.Scope, bool) { return r.cfg.Scope, !r.cfg.Scope.IsZero() }

func (r *Registry) IsAccount(ref contentref.ContentRef) bool {
	k, e := r.Kind(ref.ContentKind)
	return e == nil && k.Account && ref.TenantID == k.ns
}

func (k *RegisteredKind) root() string {
	if !k.scope.IsZero() {
		return k.scope.Root()
	}
	return k.ns + "/"
}

func (k *RegisteredKind) prefix(id, version string) string {
	if k.Versions && version != "" {
		id = version
	}
	if !k.scope.IsZero() {
		return k.scope.Prefix(k.Name, id)
	}
	return layout.Prefix(k.ns, k.Name, id)
}

// canonicalUUID accepts lowercase hyphenated UUID text of any version, except
// the nil UUID.
func canonicalUUID(s string) bool {
	if len(s) != 36 || s == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
