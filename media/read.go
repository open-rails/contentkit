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
	"slices"
	"strings"
	"time"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
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

// Reader answers reads and playlists after one trusted resolution per item.
// Whole-item grants use a folder token; restricted grants sign only approved
// immutable objects and never issue a folder cookie.
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
	// ErrEditorReadUnsupported refuses unserved editor media when a custom
	// store cannot issue a browser-reachable, exact-key editor capability.
	ErrEditorReadUnsupported = errors.New("media: store does not support authorized editor reads")
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
	Resolution resolve.Resolution
	Manifest   *Manifest
	Expires    time.Time
	item       string          // whole-item token only; never issued for a restricted grant
	files      map[string]bool // exact private blob allowlist, nil for whole-item grants
	actor      access.Actor
	editorRead bool // explicit management read, after current editor authorization
	ctx        context.Context
	r          *Reader
}

// Grant resolves ref for actor exactly once and loads its manifest. A
// resolver error denies (ErrResolve); an invisible item is ErrNotVisible. A
// visible item without a manifest has no files. A viewer with access who
// has opened too many items this hour (ReaderOptions.Issuance) gets a
// LimitError (ErrRateLimited) instead of a media grant; an anonymous
// actor with access must carry Actor.IP (ErrNoViewerKey).
func (r *Reader) Grant(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (*Grant, error) {
	item, err := r.reg.Item(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	res, err := resolve.One(ctx, r.reg.cfg.Hooks.Resolver, ref, actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	if !res.Visible {
		return nil, ErrNotVisible
	}
	scope := strings.TrimSuffix(item.Prefix(), "/")
	editorRead := res.Editor && resolve.EditorRead(ctx)
	restricted := !res.Editor && !res.ReadRestricted && res.MediaFiles != nil
	if res.Full() || res.Editor || res.ReadRestricted || len(res.MediaFiles) > 0 {
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
	if item.Kind().Versions && !editorRead {
		if !man.PublicationLayoutReady() {
			return nil, ErrPublicationLayoutRequired
		}
		if r.reg.cfg.Hooks.ReaderManifest == nil {
			return nil, fmt.Errorf("%w: version reader manifest is not configured", ErrResolve)
		}
		man, err = r.reg.cfg.Hooks.ReaderManifest(ctx, ref, actor, man)
		if err != nil {
			return nil, err
		}
		if man == nil {
			return nil, ErrNotVisible
		}
	}
	g := &Grant{Item: item, Resolution: res, Manifest: man, actor: actor, editorRead: editorRead, ctx: ctx, r: r,
		Expires: token.Expiry(r.o.Now(), r.o.Delivery.TTL, r.o.Delivery.Window)}
	if editorRead {
		if limit := r.o.Now().Add(5 * time.Minute); limit.Before(g.Expires) {
			g.Expires = limit
		}
		g.Expires = g.Expires.Truncate(time.Second)
	}
	if restricted {
		g.files = make(map[string]bool)
		for _, f := range man.Files {
			area, _, valid := layout.SplitBlobRef(f.Blob)
			if valid && area == layout.AreaPrivate && res.MediaFiles[f.Blob] && g.restrictedFile(f) {
				g.files[f.Blob] = true
			}
		}
	} else if res.Full() || res.Editor || res.ReadRestricted {
		g.item = r.ring.Sign(scope, g.Expires)
	}
	return g, nil
}

// Full reports whole-item private access. A restricted viewer can instead
// receive individual URLs through Allowed and URL without any folder token.
func (g *Grant) Full() bool { return g.item != "" }

// Editor reports an editor's grant.
func (g *Grant) Editor() bool { return g.Resolution.Editor }

// Allowed reports whether a read gives this viewer f's URL: with access,
// every listed file with a blob. Uploads are listed only with
// ServeOriginals or declared audio assets; HostOnly presets, files in download/ (Kind.Downloads),
// unattached files and frames not grabbed yet never.
func (g *Grant) Allowed(f File) bool {
	if f.Blob == "" || f.Gone {
		return false
	}
	area, _, ok := layout.SplitBlobRef(f.Blob)
	if !ok {
		return false
	}
	if g.editorRead {
		return true
	}
	if area != layout.AreaPublic && area != layout.AreaPrivate {
		return false
	}
	if area == layout.AreaPublic {
		return g.listed(f)
	}
	if g.files != nil {
		return g.files[f.Blob] && g.restrictedFile(f)
	}
	return g.Full() && f.Blob != "" && g.listed(f)
}

func (g *Grant) restrictedFile(f File) bool {
	if f.IsUpload() {
		return g.Item.Kind().AudioUpload(f) && g.listed(f)
	}
	if p := g.Item.Kind().private(f.Preset); p != nil && p.Zip != "" {
		return false
	}
	return f.Blob != "" && g.listed(f)
}

// Cookie is the item cookie for a viewer with access in cookie mode, else nil.
func (g *Grant) Cookie() *http.Cookie {
	if g.item == "" || !g.cookie() {
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
	return g.url(f.Blob, f.Download, dl, g.cookie())
}

// editorSourceURL is used only by explicit editor reads. Retained uploads
// remain inspectable independently of the viewer's ServeOriginals policy.
// Unserved originals/future objects use a separate short-lived exact-key
// storage capability; reader gateway tokens never open those areas.
func (g *Grant) editorSourceURL(f File) (string, error) {
	if !g.editorRead || !f.IsUpload() || f.Gone {
		return "", ErrNotAllowed
	}
	if f.Blob != "" {
		return g.editorURL(f.Blob)
	}
	key, err := g.Item.Staged(f.Staged)
	if err != nil {
		return "", ErrNotAllowed
	}
	return g.editorObjectURL(key)
}

// editorURL never falls back to the gateway or the worker's internal S3 URL.
// The initial grant must have verified current editor authority explicitly.
func (g *Grant) editorURL(blob string) (string, error) {
	if !g.editorRead {
		return "", ErrNotAllowed
	}
	key, err := g.Item.Blob(blob)
	if err != nil {
		return "", err
	}
	return g.editorObjectURL(key)
}

func (g *Grant) editorObjectURL(key string) (string, error) {
	if !g.editorRead {
		return "", ErrNotAllowed
	}
	store, ok := g.r.o.Manifests.store.(EditorReadStore)
	if !ok {
		return "", ErrEditorReadUnsupported
	}
	ttl := min(5*time.Minute, g.Expires.Sub(g.r.o.Now()))
	if ttl <= 0 {
		return "", ErrNotAllowed
	}
	request, err := store.PresignEditorGet(g.ctx, key, ttl)
	if err != nil {
		return "", err
	}
	return request.URL, nil
}

// cookie reports cookie delivery. An application has no browser to hold the
// cookie (a site forwarding its people's reads): its URLs carry the token.
func (g *Grant) cookie() bool {
	return g.r.o.Delivery.Mode == DeliverCookie && g.actor.Kind != auth.SubjectApplication
}

// url signs one exact approved object for restricted grants. Whole-item URLs
// are plain under the item cookie, or carry the item token. A download adds
// dl, its name: unsigned, since the token
// already opens the file and the gateway only accepts a name of the file's
// type.
func (g *Grant) url(blob, download string, dl, cookie bool) (string, error) {
	area, _, valid := layout.SplitBlobRef(blob)
	if !valid {
		return "", ErrNotAllowed
	}
	if area != layout.AreaPublic && area != layout.AreaPrivate {
		if g.editorRead {
			return g.editorURL(blob)
		}
		return "", ErrNotAllowed
	}
	key, err := g.Item.Blob(blob)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	if area == layout.AreaPublic {
		// Public delivery is unsigned, including for a locked viewer.
	} else if g.files != nil {
		if !g.files[blob] {
			return "", ErrNotAllowed
		}
		q.Set("t", g.r.ring.Sign(token.PrivateFileScope(key), g.Expires))
	} else if g.item == "" {
		return "", ErrNotAllowed
	} else if !cookie {
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

// DownloadURL serves a published public/private file as an attachment named
// name. Private downloads use an exact-file token after the caller's access
// and quota checks. Originals, future preparation and legacy download/ objects
// are never exposed through the reader gateway.
func (r *Reader) DownloadURL(item Item, f File, name string, ttl time.Duration) (string, time.Time, error) {
	key, err := item.Blob(f.Blob)
	if err != nil {
		return "", time.Time{}, err
	}
	area, _, valid := layout.SplitBlobRef(f.Blob)
	if !valid || area != layout.AreaPublic && area != layout.AreaPrivate || ttl <= 0 {
		return "", time.Time{}, fmt.Errorf("%w: %s is not a served download", ErrNotAllowed, f.Path)
	}
	exp := time.Unix(r.o.Now().Add(ttl).Unix(), 0)
	q := url.Values{}
	if area == layout.AreaPrivate {
		q.Set("t", r.ring.Sign(token.PrivateFileScope(key), exp))
	}
	if name != "" {
		q.Set("dl", name)
	}
	address := r.base + layout.URLPrefix + key
	if len(q) > 0 {
		address += "?" + q.Encode()
	}
	return address, exp, nil
}

// SignTicket signs a host route's own scope until exp with the delivery key:
// a short-lived ticket. Its scope must hold a ':', which no item or download
// scope does, so a ticket opens no media.
func (r *Reader) SignTicket(scope string, exp time.Time) string {
	if !strings.Contains(scope, ":") {
		panic("media: a ticket scope needs a ':'")
	}
	return r.ring.Sign(scope, exp)
}

// VerifyTicket checks a SignTicket ticket for scope now.
func (r *Reader) VerifyTicket(ticket, scope string) error {
	if !strings.Contains(scope, ":") {
		return token.ErrInvalid
	}
	return r.ring.VerifyScope(ticket, scope, r.o.Now())
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

// NamedURLs are, for a viewer the item opens to, the URLs of the named
// uploads' (Upload.Named) first private images, by name: how an opened
// content's body shows images that have no public files. Names without one,
// and every name for a viewer the item does not open to, are absent. The URLs
// carry the applicable exact-file or item token (cookie mode is not assumed).
func (r *Reader) NamedURLs(ctx context.Context, ref contentref.ContentRef, actor access.Actor, names []string) (map[string]string, error) {
	out := map[string]string{}
	if len(names) == 0 {
		return out, nil
	}
	g, err := r.Grant(ctx, ref, actor)
	if errors.Is(err, ErrNotVisible) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	k := g.Item.Kind()
	for _, f := range g.Manifest.Files {
		if f.IsUpload() || !g.Allowed(f) {
			continue
		}
		p := k.private(f.Preset)
		src, ok := g.Manifest.Get(f.From)
		if p == nil || p.Image == nil || p.HostOnly || !ok || !src.IsUpload() {
			continue
		}
		name := k.NameOf(src.Path)
		if _, done := out[name]; done || !slices.Contains(names, name) {
			continue
		}
		if out[name], err = g.url(f.Blob, "", false, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReadOptions select what a read returns.
type ReadOptions struct {
	Prefix        string // only files under this path prefix ("low-res/")
	Offset, Limit int    // the range that gets URLs
	// Paths selects at most 64 exact file paths to receive URLs, independent
	// of ordinal position. All allowed metadata remains in manifest order.
	// It cannot be combined with a nonzero Offset or Limit.
	Paths    []string
	Download bool // URLs serve each file as a download under its name
	// Editor adds an editor's uploads with their edit, frame, meta,
	// pending, failure, editor view and retained source URL, unattached ones
	// included. Source URLs are independent of the viewer's ServeOriginals.
	Editor bool
	// Status returns only whole-manifest processing counts, for editors.
	// It issues no URLs or editor-view work and ignores pagination/prefix.
	Status bool
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
	if o.Status {
		status, err := r.Status(ctx, ref, actor)
		if err != nil {
			return nil, nil, err
		}
		return &ReadResult{Access: AccessNone, State: status.State, Status: status, Files: []FileInfo{}}, nil, nil
	}
	var paths map[string]bool
	if len(o.Paths) > 0 {
		if len(o.Paths) > 64 || o.Offset != 0 || o.Limit != 0 {
			return nil, nil, fmt.Errorf("%w: at most 64 paths, without offset or limit", ErrInvalidRequest)
		}
		paths = make(map[string]bool, len(o.Paths))
		for _, p := range o.Paths {
			if p == "" {
				return nil, nil, fmt.Errorf("%w: empty path", ErrInvalidRequest)
			}
			paths[p] = true
		}
		o.Limit = len(paths)
	} else {
		if o.Limit == 0 {
			o.Limit = r.o.DefaultLimit
		}
		o.Limit = min(o.Limit, r.o.MaxLimit)
	}
	if o.Editor {
		ctx = resolve.WithEditorRead(ctx)
	}
	g, err := r.Grant(ctx, ref, actor)
	if err != nil {
		return nil, nil, err
	}
	editor := o.Editor && g.Editor()
	out := &ReadResult{Access: AccessNone, Offset: o.Offset, Limit: o.Limit, Expires: g.Expires.Unix(), Files: []FileInfo{}, Cookie: g.Cookie()}
	if g.Full() {
		out.Access, out.Meta = AccessFull, g.Manifest.Meta
	} else if len(g.files) > 0 {
		out.Access = AccessPartial
	}
	k := g.Item.Kind()
	if !editor || !k.Versions {
		for _, n := range k.previewNames(g.Manifest) {
			out.Previews = append(out.Previews, g.r.base+layout.URLPrefix+g.Item.PublicPrefix()+n)
		}
	}
	out.Public = r.reg.publicImages(g.Item, g.Manifest)
	if editor && k.Versions {
		out.Public, err = g.editorPublicImages()
		if err != nil {
			return nil, nil, err
		}
		for _, image := range out.Public {
			if preset := k.public(image.Preset); preset != nil && preset.First > 0 {
				for _, rendition := range image.Renditions {
					out.Previews = append(out.Previews, rendition.URL)
				}
			}
		}
	}
	if !editor && k.Publish != nil {
		out.Public = slices.DeleteFunc(out.Public, func(p PublicImage) bool { return p.Preset == k.Publish.Previews })
	}
	if editor {
		out.State, out.Full, out.Uploads = k.Readiness(g.Manifest).State, g.Manifest.Full, k.UploadRules()
		if out.State == StateReady && slices.ContainsFunc(g.Manifest.Files, func(f File) bool { return !f.Unattached && statusArchivePending(k, f) }) {
			out.State = StateProcessing
		}
	}
	views := &editorViews{g: g}
	for _, f := range g.Manifest.Files {
		if !strings.HasPrefix(f.Path, o.Prefix) {
			continue
		}
		// A viewer is listed only what it opens: without access nothing, so
		// no path, size or count tells what the item holds.
		if !editor && !g.Allowed(f) {
			out.Locked = out.Locked || f.Blob != "" && g.listed(f)
			continue
		}
		n := out.Total
		out.Total++
		// Viewers get what a file is, never editor fields: an edit, a frame's
		// source blob, meta, pending work or failures.
		fi := FileInfo{Path: f.Path, Type: f.Type, Size: f.Size, W: f.W, H: f.H, Dur: f.Dur, Download: f.Download}
		switch {
		case f.IsUpload() && editor:
			fi = uploadInfo(f, k)
		case editor:
			fi.From = f.From
		}
		sign := paths[f.Path] || paths == nil && n >= o.Offset && n < o.Offset+o.Limit
		if sign {
			switch {
			case editor && f.IsUpload() && (f.Blob != "" || f.Staged != "") && !f.Gone:
				fi.URL, err = g.editorSourceURL(f)
			case g.Allowed(f):
				fi.URL, err = g.URL(f, o.Download)
			}
			if err != nil {
				return nil, nil, err
			}
		}
		if editor && f.IsUpload() && sign {
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
		progress := r.statusProgress(ctx, ref, g.Manifest)
		out.Status = statusOf(k, g.Manifest, progress)
		if pending, admitted, err := r.o.Manifests.admittedManifest(ctx, g.Item); err != nil {
			return nil, nil, err
		} else if admitted {
			out.Status = statusOf(k, pending, EncodeStatus{})
			out.Status.State, out.State = StateProcessing, StateProcessing
		}
		r.addProgress(out.Files, progress)
	}
	return out, g, nil
}

// listed reports a file a viewer with access is listed: derived files and
// served uploads (attached only), never one in download/. An editor read
// lists every file.
func (g *Grant) listed(f File) bool {
	if area, _, ok := layout.SplitBlobRef(f.Blob); !ok || area != layout.AreaPublic && area != layout.AreaPrivate {
		return false
	}
	if f.IsUpload() {
		if f.Type == "application/zip" {
			return false
		}
		return !f.Unattached && (g.Item.Kind().ServeOriginals || g.Item.Kind().AudioUpload(f)) && f.Blob != "" && !f.Gone
	}
	if p := g.Item.Kind().private(f.Preset); p != nil && p.HostOnly {
		return false
	}
	src, ok := g.Manifest.Get(f.From)
	return !ok || !src.IsUpload() || !src.Unattached
}

// editorViews checks only the upload's recorded editor-view allocation.
type editorViews struct {
	g       *Grant
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
	key, err := e.g.Item.Blob(name)
	if err != nil {
		return "", err
	}
	if _, err = e.g.r.o.Manifests.store.Head(ctx, key); errors.Is(err, ErrNotFound) {
		e.missing = true
		return "", nil
	} else if err != nil {
		return "", err
	}
	return e.g.editorURL(name)
}

func (g *Grant) editorPublicImages() ([]PublicImage, error) {
	var images []PublicImage
	for _, file := range g.Manifest.Files {
		for _, publication := range file.Public {
			if !publication.Ready() || len(publication.Blobs) == 0 {
				continue
			}
			image := PublicImage{From: file.Path, Preset: publication.Preset}
			for i, blob := range publication.Blobs {
				address, err := g.editorURL(blob)
				if err != nil {
					return nil, err
				}
				rendition := PublicRendition{URL: address}
				if i < len(publication.Dims) {
					rendition.W, rendition.H = publication.Dims[i].W, publication.Dims[i].H
				}
				image.Renditions = append(image.Renditions, rendition)
			}
			images = append(images, image)
		}
	}
	return images, nil
}

// addProgress fills pending upload progress from the same checkpoint used by status.
func (r *Reader) addProgress(files []FileInfo, st EncodeStatus) {
	for i := range files {
		f := &files[i]
		if !f.Upload || len(f.Pending) == 0 || !isVideoType(f.Type) {
			continue
		}
		if p, ok := st.Files[f.Path]; ok {
			f.Progress = &p
		} else if st.Queued != nil {
			q := *st.Queued
			f.Progress = &q
		}
	}
}
