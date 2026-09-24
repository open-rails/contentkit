package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/token"
)

// DeliveryMode is how viewers with full access present their token.
type DeliveryMode string

const (
	// DeliverCookie (default) returns plain URLs plus a folder cookie scoped to
	// the item's blobs/, so browsers cache immutable blobs normally.
	DeliverCookie DeliveryMode = "cookie"
	// DeliverURL appends ?t= to every URL: apps and clients without cookies.
	DeliverURL DeliveryMode = "url"
)

// CookieName is the access worker's cookie.
const CookieName = token.CookieName

// Delivery is the host's per-site delivery configuration.
type Delivery struct {
	Mode DeliveryMode
	// BaseURL is the access worker origin for this site, e.g.
	// "https://media.doujins.com".
	BaseURL string
	// CookieDomain is the site's registrable domain, e.g. "doujins.com";
	// required in cookie mode, because a host-only cookie never reaches media.
	CookieDomain string
	// SigningKey is the current key of the ring the access worker verifies.
	SigningKey token.Key
	// TTL is the minimum token lifetime (default 1h); expiries round up to
	// Window (default token.DefaultWindow).
	TTL    time.Duration
	Window time.Duration
}

// Hooks are optional host callbacks.
type Hooks struct {
	// DownloadName returns the display name a download is saved under, e.g.
	// "[Artist] Title (English).zip". Default: "{content_id}-{key}{ext}".
	DownloadName func(ctx context.Context, ref contentref.ContentRef, key string, d Download) (string, error)
	// Failed reports a file a processor cannot derive (an undecodable image,
	// say); file is the manifest file name, or the slot name. The job does not
	// retry it; a new commit does.
	Failed func(ctx context.Context, ref contentref.ContentRef, file string, err error)
	// SlotEncoded reports a slot's new outputs as the stamp a host stores
	// (one value per slot) and passes to Reader.SlotOutputs for listings.
	SlotEncoded func(ctx context.Context, ref contentref.ContentRef, slot string, stamp SlotStamp)
}

// ReaderOptions configure a Reader.
type ReaderOptions struct {
	Manifests *Manifests
	Kinds     *Registry
	Resolver  access.ContentResolver
	Delivery  Delivery
	Hooks     Hooks
	// Progress adds live encode progress to pending video files; optional.
	Progress ProgressSource
	// MaxLimit caps ReadOptions.Limit (default 200); DefaultLimit is used when
	// Limit is 0 (default 50).
	MaxLimit, DefaultLimit int
	Now                    func() time.Time
}

// Reader answers the read API: one Resolve per item, metadata for every file,
// and signed URLs for the requested range.
type Reader struct {
	manifests *Manifests
	kinds     *Registry
	resolver  access.ContentResolver
	delivery  Delivery
	base      *url.URL
	ring      token.Ring
	hooks     Hooks
	progress  ProgressSource
	maxLimit  int
	defLimit  int
	now       func() time.Time
}

var (
	// ErrNotVisible hides an item the viewer may not see, or that does not exist.
	ErrNotVisible = errors.New("media: not found")
	// ErrResolve wraps a resolver failure; it always denies.
	ErrResolve = errors.New("media: resolve failed")
	// ErrInvalidRequest is a malformed read request.
	ErrInvalidRequest = errors.New("media: invalid request")
)

func NewReader(o ReaderOptions) (*Reader, error) {
	if o.Manifests == nil || o.Kinds == nil || o.Resolver == nil {
		return nil, errors.New("media: Reader needs Manifests, Kinds and Resolver")
	}
	d := o.Delivery
	if d.Mode == "" {
		d.Mode = DeliverCookie
	}
	if d.Mode != DeliverCookie && d.Mode != DeliverURL {
		return nil, fmt.Errorf("media: unknown delivery mode %q", d.Mode)
	}
	base, err := url.Parse(strings.TrimRight(d.BaseURL, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.RawQuery != "" {
		return nil, fmt.Errorf("media: Delivery.BaseURL %q must be an http(s) origin", d.BaseURL)
	}
	if d.Mode == DeliverCookie && d.CookieDomain == "" {
		return nil, errors.New("media: cookie delivery needs Delivery.CookieDomain")
	}
	ring, err := token.NewRing(d.SigningKey, nil)
	if err != nil {
		return nil, err
	}
	if d.TTL <= 0 {
		d.TTL = time.Hour
	}
	if d.Window <= 0 {
		d.Window = token.DefaultWindow
	}
	r := &Reader{manifests: o.Manifests, kinds: o.Kinds, resolver: o.Resolver, delivery: d, base: base, ring: ring,
		hooks: o.Hooks, progress: o.Progress, maxLimit: orDefault(o.MaxLimit, 200), defLimit: orDefault(o.DefaultLimit, 50), now: o.Now}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

// PublicURL is the plain URL of a public slot output or inline image; it
// reads nothing.
func (r *Reader) PublicURL(ref contentref.ContentRef, name string) (string, error) {
	item, err := r.kinds.Item(ref.Content())
	if err != nil {
		return "", err
	}
	key, err := item.Public(name)
	if err != nil {
		return "", err
	}
	return r.objectURL(key), nil
}

// Grant is one viewer's resolved access to one item or version: the read API
// and HLS playlists sign every URL through it.
type Grant struct {
	Item       Item
	Resolution access.Resolution
	Manifest   *Manifest
	Expires    time.Time
	units      int
	folder     string // folder token; full access only
	actor      access.Actor
	r          *Reader
}

// Grant resolves ref for actor exactly once and loads its manifest. A
// resolver error denies (ErrResolve); an invisible item is ErrNotVisible. A
// visible item without a manifest yet has no files.
func (r *Reader) Grant(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (*Grant, error) {
	requested, err := r.kinds.Item(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	if _, err := requested.ManifestKey(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	res, err := access.ResolveOne(ctx, r.resolver, ref, actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	if !res.Visible {
		return nil, ErrNotVisible
	}
	canon, err := canonical(ref, res.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	item, err := r.kinds.Item(canon)
	if err == nil {
		_, err = item.ManifestKey()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: resolver ref: %w", ErrResolve, err)
	}
	man, _, err := r.manifests.Get(ctx, canon)
	if errors.Is(err, ErrNotFound) {
		man = &Manifest{}
	} else if err != nil {
		return nil, err
	}
	g := &Grant{Item: item, Resolution: res, Manifest: man, units: res.Units(len(man.Files)),
		Expires: token.Expiry(r.now(), r.delivery.TTL, r.delivery.Window), actor: actor, r: r}
	if res.Full() {
		g.folder = r.ring.Sign(item.BlobsPrefix(), g.Expires)
	}
	return g, nil
}

// canonical applies the resolver's Ref: zero keeps the request; another
// tenant is refused.
func canonical(requested, resolved contentref.ContentRef) (contentref.ContentRef, error) {
	if resolved.ContentID == "" {
		return requested, nil
	}
	if resolved.TenantID == "" {
		resolved.TenantID = requested.TenantID
	}
	if resolved.ContentKind == "" {
		resolved.ContentKind = requested.ContentKind
	}
	if resolved.TenantID != requested.TenantID {
		return contentref.ContentRef{}, fmt.Errorf("resolver returned tenant %q for %s", resolved.TenantID, requested)
	}
	return resolved, nil
}

// Full reports full access: one folder token covers every blob.
func (g *Grant) Full() bool { return g.folder != "" }

// Allowed reports whether file i is served to this viewer: within the
// preview cut, or a teaser of a visible item.
func (g *Grant) Allowed(i int) bool {
	if i < 0 || i >= len(g.Manifest.Files) {
		return false
	}
	return i < g.units || g.Manifest.Files[i].Teaser()
}

// Cookie is the folder cookie to set in cookie mode with full access, else nil.
func (g *Grant) Cookie() *http.Cookie {
	if !g.Full() || g.r.delivery.Mode != DeliverCookie {
		return nil
	}
	return &http.Cookie{
		Name: CookieName, Value: g.folder,
		Domain: g.r.delivery.CookieDomain, Path: g.r.base.Path + "/" + g.Item.BlobsPrefix(),
		Expires: g.Expires, MaxAge: max(1, int(g.Expires.Sub(g.r.now()).Seconds())),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}

// ErrNotAllowed is a URL request for a file the viewer may not have, or a
// blob that file does not reference.
var ErrNotAllowed = errors.New("media: not allowed")

// Editor reports an editor's grant: EditorOnly variants are signed.
func (g *Grant) Editor() bool { return g.Resolution.Editor }

// URL signs blob of file i: plain in cookie mode with full access, the
// folder token in URL mode, else a token for exactly that key. An
// EditorOnly variant (editors only) lives in editor/, which no viewer token
// covers, and always carries an editor token.
func (g *Grant) URL(i int, blob string) (string, error) {
	if !g.Allowed(i) {
		return "", ErrNotAllowed
	}
	f := g.Manifest.Files[i]
	if !slices.Contains(fileBlobs(f), blob) {
		if !g.Editor() || !slices.Contains((&Manifest{Files: []File{f}}).EditorBlobs(), blob) {
			return "", ErrNotAllowed
		}
		key, err := g.Item.EditorBlob(blob)
		if err != nil {
			return "", err
		}
		return g.r.objectURL(key) + "?t=" + g.r.editorToken(g.Item, g.Expires), nil
	}
	key, err := g.Item.Blob(blob)
	if err != nil {
		return "", err
	}
	u := g.r.objectURL(key)
	switch {
	case g.Full() && g.r.delivery.Mode == DeliverCookie:
		return u, nil
	case g.Full():
		return u + "?t=" + g.folder, nil
	}
	return u + "?t=" + g.r.ring.Sign(token.FileScope(key), g.Expires), nil
}

// DownloadURL signs a manifest download under its display name; full access
// only, and always a URL token because the worker must see the signed dl=.
func (g *Grant) DownloadURL(ctx context.Context, key string) (name, u string, err error) {
	d, ok := g.Manifest.Downloads[key]
	if !g.Full() || !ok {
		return "", "", ErrNotAllowed
	}
	if name, err = g.r.downloadName(ctx, g.Item.Ref(), key, d); err != nil {
		return "", "", err
	}
	blob, err := g.Item.Blob(d.Blob)
	if err != nil {
		return "", "", err
	}
	tok := g.r.ring.Sign(token.DownloadScope(blob, name), g.Expires)
	return name, g.r.objectURL(blob) + "?t=" + tok + "&dl=" + url.QueryEscape(name), nil
}

func (r *Reader) downloadName(ctx context.Context, ref contentref.ContentRef, key string, d Download) (string, error) {
	if r.hooks.DownloadName != nil {
		name, err := r.hooks.DownloadName(ctx, ref, key, d)
		if err != nil || name == "" {
			return "", fmt.Errorf("media: download name for %s %q: %v", ref, key, err)
		}
		return name, nil
	}
	return ref.ContentID + "-" + key + extension(d.Type), nil
}

func extension(contentType string) string {
	switch contentType {
	case "application/zip":
		return ".zip"
	case "video/mp4":
		return ".mp4"
	case "audio/mp4":
		return ".m4a"
	}
	return ""
}

func (r *Reader) objectURL(key string) string { return r.base.String() + "/" + key }

// fileBlobs lists the blobs/ names one file references (EditorOnly
// variants are in editor/).
func fileBlobs(f File) []string {
	m := Manifest{Files: []File{f}}
	return m.Blobs()
}

// editorToken covers the item's editor/ folder: EditorOnly variants and
// unpublished poster and hover-preview outputs. Only editors get it.
func (r *Reader) editorToken(item Item, exp time.Time) string {
	return r.ring.Sign(item.EditorPrefix(), exp)
}

// ReadOptions select the URLs a read returns.
type ReadOptions struct {
	// Variants in preference order: each file in range gets a URL for the
	// first one it has (EditorOnly ones only for editors). Empty returns
	// metadata only.
	Variants      []string
	Offset, Limit int
}

// Access levels in ReadResult.
const (
	AccessFull    = "full"    // every file; downloads
	AccessPreview = "preview" // files [0, preview_limit) plus teasers
	AccessNone    = "none"    // teasers only
)

// ReadResult is the read API response. Files lists every file; only allowed
// files inside [offset, offset+limit) carry a URL, and files past the cut
// omit their name.
type ReadResult struct {
	Access       string         `json:"access"`
	Total        int            `json:"total"`
	PreviewLimit int            `json:"preview_limit"`
	Offset       int            `json:"offset"`
	Limit        int            `json:"limit"`
	Expires      int64          `json:"expires"` // unix seconds; URLs and cookie stop working then
	Meta         map[string]any `json:"meta,omitempty"`
	Files        []FileInfo     `json:"files"`
	Downloads    []DownloadInfo `json:"downloads,omitempty"`
	// Cookie must be set on the response (cookie mode, full access).
	Cookie *http.Cookie `json:"-"`
}

type FileInfo struct {
	Index    int     `json:"index"`
	Name     string  `json:"name,omitempty"`
	Type     string  `json:"type,omitempty"`
	Width    int     `json:"w,omitempty"`
	Height   int     `json:"h,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	Edit     *Edit   `json:"edit,omitempty"` // editors only: with Dims, what re-cropping needs
	Dims     *Dims   `json:"dims,omitempty"` // editors only: the source's size; w/h is the edited size
	Teaser   bool    `json:"teaser,omitempty"`
	Locked   bool    `json:"locked,omitempty"`
	HLS      bool    `json:"hls,omitempty"`
	Failed   string  `json:"failed,omitempty"` // editors only: why the file cannot be processed (video encode, image derive)
	// FailedCode and FailedDetails type an image refusal (image_too_large,
	// animation_not_allowed, …); editors only.
	FailedCode    string        `json:"failed_code,omitempty"`
	FailedDetails *ErrorDetails `json:"failed_details,omitempty"`
	// Progress of a pending encode (none yet, or a replaced source); served
	// with the file, as it reveals only timing and queue depth.
	Progress *EncodeProgress `json:"progress,omitempty"`
	Variant  string          `json:"variant,omitempty"`
	URL      string          `json:"url,omitempty"`
}

type DownloadInfo struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	Size int64  `json:"size,omitempty"`
	URL  string `json:"url"`
}

// Read resolves ref once and answers the read API.
func (r *Reader) Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o ReadOptions) (*ReadResult, error) {
	res, _, err := r.read(ctx, ref, actor, o)
	return res, err
}

func (r *Reader) read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o ReadOptions) (*ReadResult, *Grant, error) {
	if o.Offset < 0 || o.Limit < 0 {
		return nil, nil, fmt.Errorf("%w: negative offset or limit", ErrInvalidRequest)
	}
	if o.Limit == 0 {
		o.Limit = r.defLimit
	}
	o.Limit = min(o.Limit, r.maxLimit)
	g, err := r.Grant(ctx, ref, actor)
	if err != nil {
		return nil, nil, err
	}
	files := g.Manifest.Files
	out := &ReadResult{Access: AccessNone, Total: len(files), Offset: o.Offset, Limit: o.Limit,
		Expires: g.Expires.Unix(), Meta: g.Manifest.Meta, Files: make([]FileInfo, len(files)), Cookie: g.Cookie()}
	switch {
	case g.Full():
		out.Access = AccessFull
	case g.units > 0:
		out.Access, out.PreviewLimit = AccessPreview, g.units
	}
	for i, f := range files {
		fi := FileInfo{Index: i, Type: f.Type, Width: metaInt(f.Meta, "w"), Height: metaInt(f.Meta, "h"),
			Duration: metaFloat(f.Meta, "duration"), Teaser: f.Teaser(), HLS: f.HLS != nil && len(f.HLS.Video) > 0}
		if !g.Allowed(i) {
			fi.Locked = true
		} else {
			fi.Name = f.Name
			if g.Editor() {
				fi.Edit, fi.Dims = f.Edit, f.Dims
				if f.HLS != nil && f.HLS.Source == f.Source() {
					fi.Failed = f.HLS.Error
				}
				if ff := f.Failed(); ff != nil {
					fi.Failed, fi.FailedCode, fi.FailedDetails = ff.Message, ff.Code, ff.Details
				}
			}
			if i >= o.Offset && i < o.Offset+o.Limit {
				for _, v := range o.Variants {
					if vr, ok := f.Variants[v]; ok && (!vr.Editor || g.Editor()) {
						if fi.URL, err = g.URL(i, vr.Blob); err != nil {
							return nil, nil, err
						}
						fi.Variant = v
						break
					}
				}
			}
		}
		out.Files[i] = fi
	}
	r.addProgress(ctx, g, out.Files)
	if g.Full() {
		keys := make([]string, 0, len(g.Manifest.Downloads))
		for k := range g.Manifest.Downloads {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			d := g.Manifest.Downloads[k]
			name, u, err := g.DownloadURL(ctx, k)
			if err != nil {
				return nil, nil, err
			}
			out.Downloads = append(out.Downloads, DownloadInfo{Key: k, Name: name, Type: d.Type, Size: d.Size, URL: u})
		}
	}
	return out, g, nil
}

// addProgress fills Progress on allowed, pending video files. A failed
// progress read leaves it out rather than failing the read.
func (r *Reader) addProgress(ctx context.Context, g *Grant, files []FileInfo) {
	if r.progress == nil || g.Item.Kind().Video == nil {
		return
	}
	var pending []int
	for i, f := range g.Manifest.Files {
		if g.Allowed(i) && encodePending(f) {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return
	}
	st, err := r.progress.EncodeProgress(ctx, g.Item.Ref())
	if err != nil {
		return
	}
	for _, i := range pending {
		if p, ok := st.Files[g.Manifest.Files[i].Name]; ok {
			files[i].Progress = &p
		} else if st.Queued != nil {
			q := *st.Queued
			files[i].Progress = &q
		}
	}
}

// encodePending reports a video file whose current source has no ladder or
// failure recorded yet.
func encodePending(f File) bool {
	return strings.HasPrefix(f.Type, "video/") && (f.HLS == nil || f.HLS.Source != f.Source())
}

func metaFloat(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func metaInt(m map[string]any, k string) int { return int(metaFloat(m, k)) }

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
