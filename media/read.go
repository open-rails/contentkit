package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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

// DeliveryMode is how viewers with full access present their token.
type DeliveryMode string

const (
	// DeliverCookie (default) uses an item cookie when the kind permits
	// folder-wide access; otherwise it returns file-scoped URLs.
	DeliverCookie DeliveryMode = "cookie"
	// DeliverURL appends ?t= to every URL: apps and clients without cookies.
	DeliverURL DeliveryMode = "url"
)

// CookieName is the access agent's cookie.
const CookieName = token.CookieName

// Delivery is the host's signing configuration; URLs are at the
// registry's BaseURL.
type Delivery struct {
	Mode DeliveryMode
	// CookieDomain is the site's registrable domain, e.g. "doujins.ai";
	// required in cookie mode, because a host-only cookie never reaches media.
	CookieDomain string
	// SigningKey is the current key of the ring the access agent verifies.
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
	Now             func() time.Time
}

// Reader answers the read API and HLS playlists: one Resolve per item, and
// signed URLs for what the viewer may have.
type Reader struct {
	o       ReaderOptions
	reg     *Registry
	ring    token.Ring
	base    string
	indexes *indexCache
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
		indexes: newIndexCache(o.IndexCacheBytes)}, nil
}

// Grant is one viewer's resolved access to one item: reads and playlists
// sign every URL through it.
type Grant struct {
	Item       Item
	Resolution access.Resolution
	Manifest   *Manifest
	Expires    time.Time
	units      int            // the Pages uploads within the preview cut
	pages      map[string]int // each attached Pages upload's position
	item       string         // an item token, for full access
	actor      access.Actor
	r          *Reader
}

// Grant resolves ref for actor exactly once and loads its manifest. A
// resolver error denies (ErrResolve); an invisible item is ErrNotVisible. A
// visible item without a manifest has no files.
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
	man, _, err := r.o.Manifests.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		man = &Manifest{V: ManifestVersion}
	} else if err != nil {
		return nil, err
	}
	g := &Grant{Item: item, Resolution: res, Manifest: man, pages: map[string]int{}, actor: actor, r: r,
		Expires: token.Expiry(r.o.Now(), r.o.Delivery.TTL, r.o.Delivery.Window)}
	k := item.Kind()
	for _, f := range man.Files {
		if u, _, _, _, ok := k.upload(f.Path); f.IsUpload() && !f.Unattached && ok && k.Uploads[u].Pages {
			g.pages[f.Path] = len(g.pages)
		}
	}
	g.units = res.Units(len(g.pages))
	wholeItem := res.Full() && k.ServeOriginals
	for _, p := range k.Private {
		if p.HostOnly {
			wholeItem = false
			break
		}
	}
	if wholeItem {
		g.item = r.ring.Sign(token.ItemScope(ref.TenantID, ref.ContentKind, ref.ContentID), g.Expires)
	}
	return g, nil
}

// Full reports the resolver's full-access decision.
func (g *Grant) Full() bool { return g.Resolution.Full() }

// Editor reports an editor's grant.
func (g *Grant) Editor() bool { return g.Resolution.Editor }

// Allowed reports whether f is served to this viewer: full access; a page
// within the preview cut, or a file derived from one; or a teaser's. Uploads
// are served only with ServeOriginals; unattached files and frames not
// grabbed yet never are.
func (g *Grant) Allowed(f File) bool {
	if p := g.Item.Kind().private(f.Preset); !f.IsUpload() && p != nil && p.HostOnly {
		return false
	}
	return g.allowed(f)
}

func (g *Grant) allowed(f File) bool {
	src := f
	if !f.IsUpload() {
		s, ok := g.Manifest.Get(f.From)
		if ok && s.IsUpload() {
			src = s
		} else {
			src = File{}
		}
	} else if !g.Item.Kind().ServeOriginals || f.Gone {
		return false
	}
	if f.Blob == "" || src.Unattached {
		return false
	}
	if g.Full() {
		return true
	}
	if p, ok := g.pages[src.Path]; ok && p < g.units {
		return true
	}
	return src.Teaser()
}

// Cookie is the item cookie for unrestricted full access in cookie mode,
// else nil. Restricted kinds use file-scoped URLs in both delivery modes.
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

// URL signs f's blob (dl: under its download name).
func (g *Grant) URL(f File, dl bool) (string, error) {
	if !g.Allowed(f) {
		return "", ErrNotAllowed
	}
	return g.sign(f.Blob, f.Download, dl)
}

// HostURL signs a HostOnly preset for a full-access viewer. Call only from
// a trusted host route after enforcing its additional policy; never expose
// this operation through the generic read API.
func (g *Grant) HostURL(path string, dl bool) (string, error) {
	f, ok := g.Manifest.Get(path)
	if !ok || !g.Full() || f.IsUpload() || !g.allowed(f) {
		return "", ErrNotAllowed
	}
	p := g.Item.Kind().private(f.Preset)
	if p == nil || !p.HostOnly {
		return "", ErrNotAllowed
	}
	return g.sign(f.Blob, f.Download, dl)
}

// sign is the URL of the item's blob: plain under the item cookie, else
// with the item token or a file token; with dl, a download token.
func (g *Grant) sign(blob, download string, dl bool) (string, error) {
	key, err := g.Item.Blob(blob)
	if err != nil {
		return "", err
	}
	u := g.r.base + layout.URLPrefix + key
	switch {
	case dl && download != "":
		return u + "?t=" + g.r.ring.Sign(token.DownloadScope(key, download), g.Expires) + "&dl=" + url.QueryEscape(download), nil
	case g.item != "" && g.r.o.Delivery.Mode == DeliverCookie:
		return u, nil
	case g.item != "":
		return u + "?t=" + g.item, nil
	}
	return u + "?t=" + g.r.ring.Sign(token.FileScope(key), g.Expires), nil
}

// EditorView is the blob name of an image upload's editor view: the hash of
// its source and the editor spec, so a read finds it without rendering.
// It is the one private blob not named by its bytes: only the worker
// writes it, and no upload may name it (presign, put and copy refuse it).
// Only editor reads return it; the sweep removes it after the grace period
// and an editor read renders it again.
func (r *Registry) EditorView(f File) string {
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
	spec, _ := json.Marshal(r.cfg.Editor)
	out := map[string]bool{}
	for _, f := range m.Files {
		if v := editorView(f, spec); v != "" {
			out[v] = true
		}
	}
	return out
}

// ReadOptions select what a read returns.
type ReadOptions struct {
	Prefix        string // only files under this path prefix ("low-res/")
	Offset, Limit int    // the range that gets URLs
	Download      bool   // sign each file's download name into its URL
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
	switch {
	case g.Full():
		out.Access = AccessFull
	case g.units > 0:
		out.Access, out.PreviewLimit = AccessPreview, g.units
	}
	k := g.Item.Kind()
	if editor {
		out.State = k.Readiness(g.Manifest).State
	}
	views := &editorViews{g: g}
	for _, f := range g.Manifest.Files {
		if !strings.HasPrefix(f.Path, o.Prefix) || !g.listed(f, editor) {
			continue
		}
		n := out.Total
		out.Total++
		// Viewers get what a file is, never editor fields: an edit, a frame's
		// source blob, meta, pending work or failures.
		fi := FileInfo{Path: f.Path, Type: f.Type, Size: f.Size, W: f.W, H: f.H, Dur: f.Dur, Download: f.Download}
		switch {
		case f.IsUpload() && editor:
			fi = uploadInfo(f)
		case f.IsUpload():
			fi.Teaser = f.Teaser()
		default:
			fi.Teaser = g.sourceTeaser(f)
			if editor {
				fi.From = f.From
			}
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

// sourceTeaser reports a derived file of a teaser upload.
func (g *Grant) sourceTeaser(f File) bool {
	src, ok := g.Manifest.Get(f.From)
	return ok && src.Teaser()
}

// editorViews finds an item's editor views: one listing of private/ per read.
type editorViews struct {
	g       *Grant
	have    map[string]bool
	missing bool
}

func (e *editorViews) url(ctx context.Context, f File) (string, error) {
	name := e.g.r.reg.EditorView(f)
	if name == "" || f.Fail() != nil {
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
	return e.g.sign(name, "", false)
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
