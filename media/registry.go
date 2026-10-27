package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

var ErrUnknownKind = errors.New("media: unknown kind")

// maxWidth bounds a public width.
const maxWidth = 8192

// maxFirst bounds a preview's uploads.
const maxFirst = 100

// Registry is a validated Config.
type Registry struct {
	cfg      Config
	declared int // cfg.Kinds[:declared] are c's
	kinds    map[string]*Kind
	all      []*Kind // declared storage kinds
}

// NewRegistry validates c and its version reader configuration.
func NewRegistry(c Config) (*Registry, error) {
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
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" {
			return nil, fmt.Errorf("media: BaseURL %q must be an http(s) origin", c.BaseURL)
		}
	}
	if c.Editor == (Image{}) {
		c.Editor = DefaultEditor
	}
	if err := c.Editor.validate(); err != nil {
		return nil, fmt.Errorf("media: Editor: %w", err)
	}
	if c.Editor.Format != ImageFormatWebP {
		return nil, errors.New("media: Editor images use WebP")
	}
	kinds := slices.Clone(c.Kinds)
	r := &Registry{cfg: c, declared: len(c.Kinds), kinds: make(map[string]*Kind, len(kinds))}
	r.cfg.Kinds = make([]Kind, len(kinds))
	for i, k := range kinds {
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
		r.cfg.Kinds[i] = k
		r.kinds[k.Name] = &r.cfg.Kinds[i]
		r.all = append(r.all, &r.cfg.Kinds[i])
	}
	if a := c.Avatars; a != (Avatars{}) {
		k, ok := r.kinds[a.Kind]
		if !ok || k.public(a.Preset) == nil || !k.public(a.Preset).Stable {
			return nil, fmt.Errorf("media: Avatars: kind %q has no Stable public preset %q", a.Kind, a.Preset)
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
func (r *Registry) Config() Config { return r.cfg }

// Namespace is the app's own namespace.
func (r *Registry) Namespace() string { return r.cfg.Namespace }

// Kind returns a registered kind.
func (r *Registry) Kind(name string) (*Kind, error) {
	k, ok := r.kinds[name]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKind, name)
	}
	return k, nil
}

// Kinds are the declared storage kinds; reader chapters create no kinds.
func (r *Registry) Kinds() []*Kind { return slices.Clone(r.all) }

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
	kind   *Kind
	prefix string
}

// Item validates ref: a registered kind, its namespace and a UUIDv7 id. A
// Versions kind's ref names a version (a UUIDv7): a content has no item of
// its own; any other kind's has no version.
func (r *Registry) Item(ref contentref.ContentRef) (Item, error) {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return Item{}, err
	}
	if ref.TenantID != k.ns {
		return Item{}, fmt.Errorf("media: invalid ref %s: kind %q lives in namespace %q", ref, k.Name, k.ns)
	}
	if err := contentref.ValidateID(ref.ContentID); err != nil {
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
func (i Item) Kind() *Kind                { return i.kind }

// Prefix is the physical owner folder. Versioned kinds use the version ID;
// the parent content ID remains only in Ref and the catalog.
func (i Item) Prefix() string        { return i.prefix }
func (i Item) ManifestKey() string   { return i.prefix + layout.ManifestName }
func (i Item) PrivatePrefix() string { return i.prefix + layout.AreaPrivate + "/" }

// DownloadPrefix is the folder's download/: files only a download token opens.
func (i Item) DownloadPrefix() string  { return i.prefix + layout.AreaDownload + "/" }
func (i Item) PublicPrefix() string    { return i.prefix + layout.AreaPublic + "/" }
func (i Item) OriginalsPrefix() string { return i.prefix + layout.AreaOriginals + "/" }
func (i Item) FuturePrefix() string    { return i.prefix + layout.AreaFuture + "/" }
func (i Item) TempPrefix() string {
	if i.kind.Versions {
		return i.prefix + layout.AreaTmp + "/"
	}
	return i.prefix + layout.AreaTemp + "/"
}

// Blob is the key of an allocation by its reference: "sha256-{hex}-{uuid}"
// in private/, "download/sha256-{hex}-{uuid}" in download/.
func (i Item) Blob(ref string) (string, error) {
	area, name, ok := layout.SplitBlobRef(ref)
	if !ok {
		return "", fmt.Errorf("media: invalid blob reference %q", ref)
	}
	return i.prefix + area + "/" + name, nil
}

// BlobRef is the reference of key, an allocation in the folder.
func (i Item) BlobRef(key string) (string, bool) {
	k, ok := layout.Parse(key)
	if !ok || k.Folder() != i.prefix || !layout.ValidBlobRef(layout.BlobRef(k.Area, k.Name)) {
		return "", false
	}
	return layout.BlobRef(k.Area, k.Name), true
}

// Staged is the key of a staged upload ("u-{uuid}"), in temp/ until placed.
func (i Item) Staged(name string) (string, error) {
	if !layout.ValidStagedName(name) {
		return "", fmt.Errorf("media: invalid staged upload name %q", name)
	}
	return i.TempPrefix() + name, nil
}

// Public is the key of a public name.
func (i Item) Public(name string) (string, error) {
	if !layout.ValidPublicName(name) {
		return "", fmt.Errorf("media: invalid public name %q", name)
	}
	return i.PublicPrefix() + name, nil
}

// PublicationKey locates one prepared public-preset rendition. Versions keep
// their hash-named preparation in future; non-version assets retain their
// independent public-preset lifecycle.
func (i Item) PublicationKey(p Publication, index int) (string, error) {
	if index < 0 || index >= len(p.Names) {
		return "", fmt.Errorf("media: invalid publication index")
	}
	if len(p.Blobs) > 0 {
		return i.Blob(p.Blobs[index])
	}
	return i.Public(p.NamesOnDisk()[index])
}

// DefaultKey is the key of the kind's default public image name.
func (k *Kind) DefaultKey(name string) string {
	return k.prefix(layout.DefaultID, "") + layout.AreaPublic + "/" + name
}

// NS is the namespace the kind's items live in.
func (k *Kind) NS() string { return k.ns }

// PublicURL is the URL of item ref's public name at the registry's BaseURL.
func (r *Registry) PublicURL(ref contentref.ContentRef, name string) string {
	item, err := r.Item(ref)
	if err != nil {
		return ""
	}
	return strings.TrimRight(r.cfg.BaseURL, "/") + layout.URLPrefix + item.Prefix() + layout.AreaPublic + "/" + name
}

// PresetRules lists every kind's public presets, in registry order.
func (r *Registry) PresetRules() []PresetRule {
	out := []PresetRule{}
	for _, k := range r.all {
		for _, p := range k.Public {
			out = append(out, PresetRule{Kind: k.Name, Name: p.Name, From: p.From, Base: strings.TrimRight(r.cfg.BaseURL, "/"),
				Namespace: k.ns, Root: k.root(), To: p.To, Widths: append([]int{}, p.Widths...), Aspect: p.Image.Aspect, MinWidth: p.Image.MinWidth, First: p.First,
				Default: p.hasDefault(), Stable: p.Stable})
		}
	}
	return out
}

// MarshalJSON is the registry as data (no hooks, defaults, Choose or
// derived kinds, which NewRegistry derives again): the stock worker's
// MEDIA_KINDS_FILE.
func (r *Registry) MarshalJSON() ([]byte, error) {
	cfg := r.cfg
	cfg.Kinds = cfg.Kinds[:r.declared]
	return json.Marshal(cfg)
}

// ParseConfig reads a registry written by Registry.MarshalJSON; unknown
// keys are ignored.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("media: registry JSON: %w", err)
	}
	return c, nil
}

// Scope identifies the registry's trusted app owner; false means legacy mode.
func (r *Registry) Scope() (layout.Scope, bool) { return r.cfg.Scope, !r.cfg.Scope.IsZero() }
func (r *Registry) IsAccount(ref contentref.ContentRef) bool {
	k, e := r.Kind(ref.ContentKind)
	return e == nil && k.Account && ref.TenantID == k.ns
}
func (k *Kind) root() string {
	if !k.scope.IsZero() {
		return k.scope.Root()
	}
	return k.ns + "/"
}
func (k *Kind) prefix(id, version string) string {
	if k.Versions && version != "" {
		id, version = version, ""
	}
	return k.nestedPrefix(id, version)
}

// nestedPrefix is the historical path shape, used only to verify migrations
// and cleanup receipts. A version's live storage owner is its version ID.
func (k *Kind) nestedPrefix(id, version string) string {
	if !k.scope.IsZero() {
		return k.scope.Prefix(k.Name, id, version)
	}
	return layout.Prefix(k.ns, k.Name, id, version)
}
