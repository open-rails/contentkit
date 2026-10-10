package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// DeliveryMode is how viewers with access present their item token.
type DeliveryMode string

const (
	// DeliverCookie (default) sets the item cookie; URLs are plain.
	DeliverCookie DeliveryMode = "cookie"
	// DeliverURL appends ?t= to every URL: apps and clients without cookies.
	DeliverURL DeliveryMode = "url"
)

// CookieName is the media gateway's cookie.
const CookieName = token.CookieName

// Delivery is the host's signing configuration; URLs are at the
// registry's BaseURL.
type Delivery struct {
	Mode DeliveryMode
	// CookieDomain is the site's registrable domain, e.g. "doujins.ai";
	// required in cookie mode, because a host-only cookie never reaches media.
	CookieDomain string
	// SigningKey is the current key of the ring the media gateway verifies.
	SigningKey token.Key
	// TTL is the minimum token lifetime (default 1h); expiries round up to
	// Window (default token.DefaultWindow, whole seconds).
	TTL, Window time.Duration
}

// ReaderOptions configure a Reader.
type ReaderOptions struct {
	Manifests *Manifests // its Registry's BaseURL and Hooks.Resolver are required
	Delivery  Delivery
	// Progress adds live encode progress to pending uploads in editor
	// reads; optional.
	Progress ProgressSource
	// Queue renders the editor views an editor read finds missing; optional.
	Queue ProcessQueue
	// MaxLimit caps ReadOptions.Limit (default 200); DefaultLimit is used
	// when Limit is 0 (default 50).
	MaxLimit, DefaultLimit int
	// IndexCacheBytes bounds the track index blobs kept for playlists;
	// default 16 MiB.
	IndexCacheBytes int64
	// Issuance limits the items each viewer is given access to per hour
	// (default 120 per account, 600 per anonymous IP).
	Issuance Issuance
	Now      func() time.Time
}

// Reader answers the read API and HLS playlists: one Resolve per item. An
// item's private files are all or nothing: a viewer with access gets one
// token that opens every one of them; anyone else gets none, and sees only
// the item's public files (covers, previews).
type Reader struct {
	o       ReaderOptions
	reg     *Registry
	ring    token.Ring
	base    string
	indexes *indexCache
	issue   *issuance
}

var (
	// ErrNotVisible hides an item the viewer may not see, or that does not exist.
	ErrNotVisible = errors.New("media: not found")
	// ErrResolve wraps a resolver failure; it always denies.
	ErrResolve = errors.New("media: resolve failed")
	// ErrInvalidRequest is a malformed read request.
	ErrInvalidRequest = errors.New("media: invalid request")
	// ErrNotAllowed is a file the viewer may not have.
	ErrNotAllowed = errors.New("media: not allowed")
)

func NewReader(o ReaderOptions) (*Reader, error) {
	if o.Manifests == nil {
		return nil, errors.New("media: Reader needs Manifests")
	}
	reg := o.Manifests.Registry()
	if reg.cfg.BaseURL == "" || reg.cfg.Hooks.Resolver == nil {
		return nil, errors.New("media: Reader needs the registry's BaseURL and Hooks.Resolver")
	}
	d := &o.Delivery
	if d.Mode == "" {
		d.Mode = DeliverCookie
	}
	if d.Mode != DeliverCookie && d.Mode != DeliverURL {
		return nil, fmt.Errorf("media: unknown delivery mode %q", d.Mode)
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
	if d.Window%time.Second != 0 {
		return nil, errors.New("media: Delivery.Window must be a whole number of seconds")
	}
	if o.MaxLimit <= 0 {
		o.MaxLimit = 200
	}
	if o.DefaultLimit <= 0 {
		o.DefaultLimit = 50
	}
	if o.IndexCacheBytes <= 0 {
		o.IndexCacheBytes = 16 << 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Reader{o: o, reg: reg, ring: ring, base: strings.TrimRight(reg.cfg.BaseURL, "/"),
		indexes: newIndexCache(o.IndexCacheBytes), issue: newIssuance(o.Issuance, o.Now, slog.Default())}, nil
}

// Grant is one viewer's resolved access to one item: reads and playlists
// sign every URL through it.
type Grant struct {
	Item       Item
	Resolution access.Resolution
	Manifest   *Manifest
	Expires    time.Time
	item       string // the item token; "" without access
	actor      access.Actor
	r          *Reader
}

// Grant resolves ref for actor exactly once and loads its manifest. A
// resolver error denies (ErrResolve); an invisible item is ErrNotVisible. A
// visible item without a manifest has no files. A viewer with access who
// has opened too many items this hour (ReaderOptions.Issuance) gets a
// LimitError (ErrRateLimited) instead of the item's token; an anonymous
// actor with access must carry Actor.IP (ErrNoViewerKey).
func (r *Reader) Grant(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (*Grant, error) {
	item, err := r.reg.Item(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	res, err := access.ResolveOne(ctx, r.reg.cfg.Hooks.Resolver, ref, actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	if !res.Visible {
		return nil, ErrNotVisible
	}
	scope := token.ItemScope(ref.TenantID, ref.ContentKind, ref.ContentID)
	if res.Full() || res.Editor {
		if err := r.issue.allow(ctx, actor, res.Editor, scope); err != nil {
			return nil, err
		}
	}
	man, _, err := r.o.Manifests.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		man = &Manifest{V: ManifestVersion}
	} else if err != nil {
		return nil, err
	}
	g := &Grant{Item: item, Resolution: res, Manifest: man, actor: actor, r: r,
		Expires: token.Expiry(r.o.Now(), r.o.Delivery.TTL, r.o.Delivery.Window)}
	if res.Full() || res.Editor {
		g.item = r.ring.Sign(scope, g.Expires)
	}
	return g, nil
}

// Full reports access to the item's private files: the resolver's verdict,
// or an editor's.
func (g *Grant) Full() bool { return g.item != "" }

// Editor reports an editor's grant.
func (g *Grant) Editor() bool { return g.Resolution.Editor }

// Allowed reports whether a read gives this viewer f's URL: with access,
// every listed file with a blob. Uploads are listed only with
// ServeOriginals, HostOnly presets never (Grant.HostURL), unattached files
// and frames not grabbed yet never.
func (g *Grant) Allowed(f File) bool {
	return g.Full() && f.Blob != "" && g.listed(f, false)
}

// Cookie is the item cookie for a viewer with access in cookie mode, else nil.
func (g *Grant) Cookie() *http.Cookie {
	if g.item == "" || g.r.o.Delivery.Mode != DeliverCookie {
		return nil
	}
	return &http.Cookie{
		Name: CookieName, Value: g.item,
		Domain: g.r.o.Delivery.CookieDomain, Path: layout.URLPrefix + g.Item.PrivatePrefix(),
		Expires: g.Expires, MaxAge: max(1, int(g.Expires.Sub(g.r.o.Now()).Seconds())),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}

// URL is f's URL (dl: served as a download under its name).
func (g *Grant) URL(f File, dl bool) (string, error) {
	if !g.Allowed(f) {
		return "", ErrNotAllowed
	}
	return g.url(f.Blob, f.Download, dl, g.r.o.Delivery.Mode == DeliverCookie)
}

// HostURL is the URL of a HostOnly preset's file for a viewer with access,
// always carrying the item token (the host route may not have set the
// cookie). HostOnly only keeps a file out of generic reads: call this from a
// host route after its own policy, which decides who is handed the URL.
func (g *Grant) HostURL(path string, dl bool) (string, error) {
	f, ok := g.Manifest.Get(path)
	if !ok || !g.Full() || f.IsUpload() || f.Blob == "" {
		return "", ErrNotAllowed
	}
	if src, ok := g.Manifest.Get(f.From); ok && src.IsUpload() && src.Unattached {
		return "", ErrNotAllowed
	}
	p := g.Item.Kind().private(f.Preset)
	if p == nil || !p.HostOnly {
		return "", ErrNotAllowed
	}
	return g.url(f.Blob, f.Download, dl, false)
}

// url is the URL of the item's blob: plain under the item cookie, else with
// the item token. A download adds dl, its name: unsigned, since the token
// already opens the file and the gateway only accepts a name of the file's
// type.
func (g *Grant) url(blob, download string, dl, cookie bool) (string, error) {
	key, err := g.Item.Blob(blob)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	if !cookie {
		q.Set("t", g.item)
	}
	if dl && download != "" {
		q.Set("dl", download)
	}
	u := g.r.base + layout.URLPrefix + key
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u, nil
}

// EditorView returns the upload's recorded view when its source/spec still
// matches. Only editor reads list it; presign, put and copy cannot adopt it.
func (r *Registry) EditorView(f File) string {
	if f.Editor == nil || f.Editor.FP != r.EditorFingerprint(f) {
		return ""
	}
	return f.Editor.Blob
}

// EditorFingerprint identifies the unedited source and current editor spec.
func (r *Registry) EditorFingerprint(f File) string {
	spec, _ := json.Marshal(r.cfg.Editor)
	return editorView(f, spec)
}

func editorView(f File, spec []byte) string {
	if !f.IsUpload() || f.Blob == "" || f.Gone || !isImageType(f.Type) {
		return ""
	}
	sum := sha256.Sum256([]byte(f.Blob + "|" + string(spec)))
	return layout.SHA256Name(sum[:])
}

// editorViews are the names of m's editor views.
func (r *Registry) editorViews(m *Manifest) map[string]bool {
	out := map[string]bool{}
	for _, f := range m.Files {
		if f.Editor != nil {
			out[f.Editor.Blob] = true
		}
	}
	return out
}

// ReadOptions select what a read returns.
type ReadOptions struct {
	Prefix        string // only files under this path prefix ("low-res/")
	Offset, Limit int    // the range that gets URLs
	Download      bool   // URLs serve each file as a download under its name
	// Editor adds an editor's uploads with their edit, frame, meta,
	// pending, failure and editor view, unattached ones included.
	Editor bool
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
		o.Limit = r.o.DefaultLimit
	}
	o.Limit = min(o.Limit, r.o.MaxLimit)
	g, err := r.Grant(ctx, ref, actor)
	if err != nil {
		return nil, nil, err
	}
	editor := o.Editor && g.Editor()
	out := &ReadResult{Access: AccessNone, Offset: o.Offset, Limit: o.Limit, Expires: g.Expires.Unix(),
		Meta: g.Manifest.Meta, Files: []FileInfo{}, Cookie: g.Cookie()}
	if g.Full() {
		out.Access = AccessFull
	}
	k := g.Item.Kind()
	for _, n := range k.previewNames(g.Manifest) {
		out.Previews = append(out.Previews, g.r.base+layout.URLPrefix+g.Item.PublicPrefix()+n)
	}
	out.Public = r.reg.publicImages(g.Item, g.Manifest)
	if editor {
		out.State, out.Full, out.Uploads = k.Readiness(g.Manifest).State, g.Manifest.Full, k.UploadRules()
	}
	views := &editorViews{g: g}
	for _, f := range g.Manifest.Files {
		if !strings.HasPrefix(f.Path, o.Prefix) || !g.listed(f, editor) {
			continue
		}
		n := out.Total
		out.Total++
		// Viewers get what a file is, never editor fields: an edit, a frame's
		// source blob, meta, pending work or failures. Without access a file
		// is its path, type and size, locked: nothing names a blob.
		fi := FileInfo{Path: f.Path, Type: f.Type, Size: f.Size}
		if g.Full() {
			fi.W, fi.H, fi.Dur, fi.Download = f.W, f.H, f.Dur, f.Download
		}
		switch {
		case f.IsUpload() && editor:
			fi = uploadInfo(f)
		case editor:
			fi.From = f.From
		}
		if !g.Allowed(f) {
			fi.Locked = g.listed(f, false)
		} else if n >= o.Offset && n < o.Offset+o.Limit {
			if fi.URL, err = g.URL(f, o.Download); err != nil {
				return nil, nil, err
			}
		}
		if editor && f.IsUpload() && n >= o.Offset && n < o.Offset+o.Limit {
			if fi.EditorURL, err = views.url(ctx, f); err != nil {
				return nil, nil, err
			}
		}
		if f.Track != nil && (f.Track.Kind == TrackVideo || f.Track.Kind == TrackAudio) && g.Allowed(f) {
			if dir := path.Dir(f.Path) + "/"; len(out.HLS) == 0 || out.HLS[len(out.HLS)-1] != dir {
				out.HLS = append(out.HLS, dir)
			}
		}
		out.Files = append(out.Files, fi)
	}
	if views.missing && r.o.Queue != nil {
		_ = r.o.Queue.Enqueue(ctx, ProcessJob{Ref: ref, Editor: true}) // best effort: the next read asks again
	}
	if editor {
		r.addProgress(ctx, g, out.Files)
	}
	return out, g, nil
}

// PublicImages loads only ready, currently owned public generations. It does
// not issue private grants or reveal unpublished allocations. A missing item
// has no published images.
func (m *Manifests) PublicImages(ctx context.Context, ref contentref.ContentRef) ([]PublicImage, error) {
	man, _, err := m.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	item, _ := m.reg.Item(ref) // Get already validated the ref
	return m.reg.publicImages(item, man), nil
}

func (r *Registry) publicImages(item Item, m *Manifest) []PublicImage {
	var images []PublicImage
	for _, f := range m.Files {
		if !f.IsUpload() {
			continue
		}
		for _, p := range item.Kind().PublicFor(f.Path) {
			pub, ok := item.Kind().Publication(m, f, p)
			if !ok || !pub.Ready() {
				continue
			}
			image := PublicImage{From: f.Path, Preset: p.Name}
			for i, name := range pub.NamesOnDisk() {
				image.Renditions = append(image.Renditions, PublicRendition{
					URL: r.PublicURL(item.Ref(), name), W: pub.Dims[i].W, H: pub.Dims[i].H})
			}
			images = append(images, image)
		}
	}
	return images
}

// listed reports a file a read lists: derived files and served uploads to
// viewers (attached only); every file to an editor read.
func (g *Grant) listed(f File, editor bool) bool {
	if editor {
		return true
	}
	if f.IsUpload() {
		return !f.Unattached && g.Item.Kind().ServeOriginals && f.Blob != "" && !f.Gone
	}
	if p := g.Item.Kind().private(f.Preset); p != nil && p.HostOnly {
		return false
	}
	src, ok := g.Manifest.Get(f.From)
	return !ok || !src.IsUpload() || !src.Unattached
}

// editorViews finds an item's editor views: one listing of private/ per read.
type editorViews struct {
	g       *Grant
	have    map[string]bool
	missing bool
}

func (e *editorViews) url(ctx context.Context, f File) (string, error) {
	name := e.g.r.reg.EditorView(f)
	if e.g.r.reg.EditorFingerprint(f) == "" || f.Fail() != nil {
		return "", nil
	}
	if name == "" {
		e.missing = true
		return "", nil
	}
	if e.have == nil {
		e.have = map[string]bool{}
		for o, err := range e.g.r.o.Manifests.store.List(ctx, e.g.Item.PrivatePrefix()) {
			if err != nil {
				return "", err
			}
			e.have[strings.TrimPrefix(o.Key, e.g.Item.PrivatePrefix())] = true
		}
	}
	if !e.have[name] {
		e.missing = true
		return "", nil
	}
	return e.g.url(name, "", false, e.g.r.o.Delivery.Mode == DeliverCookie)
}

// addProgress fills Progress on pending video and audio uploads. A failed
// progress read leaves it out rather than failing the read.
func (r *Reader) addProgress(ctx context.Context, g *Grant, files []FileInfo) {
	if r.o.Progress == nil {
		return
	}
	var pending []int
	for i, f := range files {
		if f.Upload && len(f.Pending) > 0 && (isVideoType(f.Type) || isAudioType(f.Type)) {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return
	}
	st, err := r.o.Progress.EncodeProgress(ctx, g.Item.Ref())
	if err != nil {
		return
	}
	for _, i := range pending {
		if p, ok := st.Files[files[i].Path]; ok {
			files[i].Progress = &p
		} else if st.Queued != nil {
			q := *st.Queued
			files[i].Progress = &q
		}
	}
}
