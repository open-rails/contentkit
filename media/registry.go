package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

var ErrUnknownKind = errors.New("media: unknown kind")

// maxWidth bounds a public width.
const maxWidth = 8192

// Registry is a validated Config.
type Registry struct {
	cfg   Config
	kinds map[string]*Kind
}

// NewRegistry validates c.
func NewRegistry(c Config) (*Registry, error) {
	if !layout.ValidSegment(c.Namespace) {
		return nil, fmt.Errorf("media: invalid namespace %q", c.Namespace)
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" {
			return nil, fmt.Errorf("media: BaseURL %q must be an http(s) origin", c.BaseURL)
		}
	}
	if c.Hooks.PurgePublic != nil && c.BaseURL == "" {
		return nil, errors.New("media: Hooks.PurgePublic needs BaseURL")
	}
	if c.Editor == (Image{}) {
		c.Editor = DefaultEditor
	}
	if err := c.Editor.validate(); err != nil {
		return nil, fmt.Errorf("media: Editor: %w", err)
	}
	r := &Registry{cfg: c, kinds: make(map[string]*Kind, len(c.Kinds))}
	r.cfg.Kinds = make([]Kind, len(c.Kinds))
	for i, k := range c.Kinds {
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
		r.cfg.Kinds[i] = k
		r.kinds[k.Name] = &r.cfg.Kinds[i]
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

// Namespaces are the namespaces the app's items live in: its own, then the
// shared ones it imports.
func (r *Registry) Namespaces() []string {
	out := []string{r.cfg.Namespace}
	for _, k := range r.cfg.Kinds {
		if !slices.Contains(out, k.ns) {
			out = append(out, k.ns)
		}
	}
	return out
}

// Ref is the ref of item id of kind, in the kind's namespace.
func (r *Registry) Ref(kind, id string) (contentref.ContentRef, error) {
	k, err := r.Kind(kind)
	if err != nil {
		return contentref.ContentRef{}, err
	}
	ref := contentref.New(k.ns, kind, id)
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

// Item validates ref: a registered kind, its namespace and a UUIDv7 id,
// without a version (an item is the host's version).
func (r *Registry) Item(ref contentref.ContentRef) (Item, error) {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return Item{}, err
	}
	if ref.TenantID != k.ns || ref.ContentVersionID != nil {
		return Item{}, fmt.Errorf("media: invalid ref %s: kind %q lives in namespace %q, without versions", ref, k.Name, k.ns)
	}
	if err := contentref.ValidateID(ref.ContentID); err != nil {
		return Item{}, fmt.Errorf("media: invalid ref %s: %w", ref, err)
	}
	return Item{ref: ref, kind: k, prefix: layout.Prefix(k.ns, k.Name, ref.ContentID)}, nil
}

func (i Item) Ref() contentref.ContentRef { return i.ref }
func (i Item) Kind() *Kind                { return i.kind }

// Prefix is the folder, "{namespace}/{kind}/{id}/".
func (i Item) Prefix() string        { return i.prefix }
func (i Item) ManifestKey() string   { return i.prefix + layout.ManifestName }
func (i Item) PrivatePrefix() string { return i.prefix + layout.AreaPrivate + "/" }
func (i Item) PublicPrefix() string  { return i.prefix + layout.AreaPublic + "/" }
func (i Item) TempPrefix() string    { return i.prefix + layout.AreaTemp + "/" }

// Blob is the key of a private blob ("sha256-{hex}").
func (i Item) Blob(name string) (string, error) {
	if !layout.ValidHashName(name) {
		return "", fmt.Errorf("media: invalid blob name %q", name)
	}
	return i.PrivatePrefix() + name, nil
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
	if !layout.ValidSegment(name) {
		return "", fmt.Errorf("media: invalid public name %q", name)
	}
	return i.PublicPrefix() + name, nil
}

// DefaultKey is the key of the kind's default public image name.
func (k *Kind) DefaultKey(name string) string {
	return layout.Prefix(k.ns, k.Name, layout.DefaultID) + layout.AreaPublic + "/" + name
}

// NS is the namespace the kind's items live in.
func (k *Kind) NS() string { return k.ns }

// PublicURL is a public file's URL: {base}/v1/{namespace}/{kind}/{id}/public/{name}.
func PublicURL(base, namespace, kind, id, name string) string {
	return strings.TrimRight(base, "/") + layout.URLPrefix + layout.Prefix(namespace, kind, id) + layout.AreaPublic + "/" + name
}

// SrcSet is a srcset over a public template's widths: "{url} 230w, …".
func SrcSet(base, namespace, kind, id, to string, widths []int) string {
	set := make([]string, len(widths))
	for i, w := range widths {
		n := strconv.Itoa(w)
		set[i] = PublicURL(base, namespace, kind, id, fill(to, map[string]string{"w": n})) + " " + n + "w"
	}
	return strings.Join(set, ", ")
}

// PublicURL is the URL of item ref's public name at the registry's BaseURL.
func (r *Registry) PublicURL(ref contentref.ContentRef, name string) string {
	return PublicURL(r.cfg.BaseURL, ref.TenantID, ref.ContentKind, ref.ContentID, name)
}

// SrcSet is the srcset of ref's public preset (by name), "" when the kind has none.
func (r *Registry) SrcSet(ref contentref.ContentRef, preset string) string {
	k, err := r.Kind(ref.ContentKind)
	if err != nil {
		return ""
	}
	p := k.public(preset)
	if p == nil {
		return ""
	}
	return SrcSet(r.cfg.BaseURL, ref.TenantID, ref.ContentKind, ref.ContentID, p.To, p.Widths)
}

// MarshalJSON is the registry as data (no hooks, defaults or Choose): the
// stock worker's MEDIA_KINDS_FILE.
func (r *Registry) MarshalJSON() ([]byte, error) { return json.Marshal(r.cfg) }

// ParseConfig reads a registry written by Registry.MarshalJSON; unknown
// keys are ignored.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("media: registry JSON: %w", err)
	}
	return c, nil
}
