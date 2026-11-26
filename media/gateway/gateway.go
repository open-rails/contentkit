// Package gateway serves each media host's bucket: one app's, since the
// bucket is the tenancy boundary. URLs are /v1/{kind}/{id}/{area}/{sha256},
// and /v1/_accounts/{kind}/{account}/… for the tenant's account media, read
// from the bucket its Route names for them (the avatar primary's).
//
// public/ is served to anyone; private/ needs the item's token, carried by
// the cookie path-scoped to that item's private/ (or ?t= for applications);
// internal/, tmp/ and the manifest are never served. Every stored name is
// the hash of its bytes, so a served file never changes: browsers and CDNs
// keep it. Denials use a uniform no-store 404 and token denials precede S3
// access. Before serving, one HEAD of the item's fence (layout.FenceName)
// rejects a soft-deleted or erased item, including through old tokens.
// Forbidden fence reads fail closed. The gateway reads no manifests or
// databases.
package gateway

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

const (
	// Hash-named files never change, so a browser keeps its copy. Tokens and
	// the deletion fence guard new reads, not bytes a browser already holds.
	publicCacheControl  = "public, max-age=31536000, immutable"
	privateCacheControl = "private, max-age=31536000, immutable"
)

// Route is one media host: its app's bucket and, with Accounts, the bucket
// holding the tenant's account media (layout.AccountRoot), the avatar
// primary's; empty serves none.
type Route struct {
	Host     string   `json:"host"`
	Bucket   string   `json:"bucket"`
	Accounts string   `json:"accounts,omitempty"`
	Origins  []string `json:"origins,omitempty"`
}

// Config configures a Handler.
type Config struct {
	Routes []Route

	Endpoint, Region             string // path-style S3 endpoint; Region default us-east-1
	AccessKeyID, SecretAccessKey string // read key for served private/public objects, including the fence HEAD; scoped prefixes need missing-object 404s
	Ring                         token.Ring
	Client                       *http.Client // default: a tuned transport without compression
	Logger                       *slog.Logger
}

// Handler serves media objects.
type Handler struct {
	cfg    Config
	base   *url.URL
	creds  aws.Credentials
	signer *v4.Signer
}

var emptySHA256 = hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }())

// New validates cfg.
func New(cfg Config) (*Handler, error) {
	if cfg.Endpoint == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("gateway: Endpoint and S3 credentials are required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("gateway: invalid Endpoint")
	}
	if len(cfg.Routes) == 0 {
		return nil, errors.New("gateway: at least one Route is required")
	}
	cfg.Routes = slices.Clone(cfg.Routes)
	for i := range cfg.Routes {
		cfg.Routes[i].Origins = slices.Clone(cfg.Routes[i].Origins)
	}
	seen := map[string]bool{}
	for _, r := range cfg.Routes {
		if r.Host == "" || strings.Trim(r.Host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" || !layout.ValidSegment(r.Bucket) ||
			r.Accounts != "" && !layout.ValidSegment(r.Accounts) || seen[r.Host] {
			return nil, errors.New("gateway: invalid or duplicate Route")
		}
		seen[r.Host] = true
		for _, origin := range r.Origins {
			if err := validOrigin(origin); err != nil {
				return nil, err
			}
		}
	}
	cfg.Region = cmp.Or(cfg.Region, "us-east-1")
	if cfg.Client == nil {
		cfg.Client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableCompression:    true, // pass bytes, lengths and ranges through untouched
		}}
	}
	cfg.Logger = cmp.Or(cfg.Logger, slog.Default())
	return &Handler{
		cfg:    cfg,
		base:   base,
		creds:  aws.Credentials{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey},
		signer: v4.NewSigner(),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.URL.RawPath == "" {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	h.cors(w, r)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		h.fail(w, http.StatusMethodNotAllowed)
		return
	}
	o, ok := h.parse(r)
	if !ok {
		h.fail(w, http.StatusNotFound)
		return
	}
	key := o.key()
	cacheControl := publicCacheControl
	var dl *string // a private file's unsigned download name
	if o.area != layout.AreaPublic {
		if err := h.authorize(r, o.bucket, key); err != nil {
			h.cfg.Logger.Debug("media-gateway: denied", "key", key, "reason", err)
			h.fail(w, http.StatusNotFound)
			return
		}
		cacheControl = privateCacheControl
	}
	if q := r.URL.Query(); q.Has("dl") {
		dl = new(q.Get("dl"))
	}
	check := r.Clone(r.Context())
	check.Method = http.MethodHead
	check.Header = make(http.Header)
	status, err := h.fetch(check, o.bucket, o.folder+layout.AreaPublic+"/"+layout.FenceName)
	if err != nil {
		h.fail(w, http.StatusBadGateway)
		return
	}
	code := status.StatusCode
	_ = status.Body.Close()
	if code != http.StatusNotFound {
		if code == http.StatusOK {
			h.fail(w, http.StatusNotFound)
		} else {
			h.fail(w, http.StatusBadGateway)
		}
		return
	}
	resp, err := h.fetch(r, o.bucket, key)
	if err != nil {
		if r.Context().Err() == nil {
			h.cfg.Logger.Error("media-gateway: upstream", "key", key, "err", err)
		}
		h.fail(w, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	h.relay(w, r, key, resp, cacheControl, dl, o.area == layout.AreaPublic)
}

type object struct {
	folder, area, name string
	route              *Route
	bucket             string
}

func (o object) key() string { return o.folder + o.area + "/" + o.name }

// parse accepts exactly a served /v1/ object path, unescaped, on a Route's
// host: account media only where the Route names their bucket.
func (h *Handler) parse(r *http.Request) (object, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, layout.URLPrefix)
	if !ok || r.URL.RawPath != "" {
		return object{}, false
	}
	k, ok := layout.Parse(rest)
	if !ok || k.Name == layout.FenceName || strings.HasPrefix(k.ID, "_") && !(k.ID == layout.DefaultID && k.Area == layout.AreaPublic) {
		return object{}, false
	}
	if !layout.Served(k.Area) {
		return object{}, false
	}
	host := r.Host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.ToLower(host)
	i := slices.IndexFunc(h.cfg.Routes, func(route Route) bool { return route.Host == host })
	if i < 0 {
		return object{}, false
	}
	o := object{folder: k.Folder(), area: k.Area, name: k.Name, route: &h.cfg.Routes[i], bucket: h.cfg.Routes[i].Bucket}
	if k.Account {
		o.bucket = o.route.Accounts
	}
	return o, o.bucket != ""
}

var errNoToken = errors.New("no token")

// authorize accepts the item's token for bucket from the URL or an mt cookie
// (a browser may send several).
func (h *Handler) authorize(r *http.Request, bucket, key string) (err error) {
	now := time.Now()
	var toks []string
	if t := r.URL.Query().Get("t"); t != "" {
		toks = append(toks, t)
	}
	for _, c := range r.CookiesNamed(token.CookieName) {
		toks = append(toks, c.Value)
	}
	for _, tok := range toks {
		e := h.cfg.Ring.VerifyPrivate(tok, bucket, key, now)
		if e == nil {
			return nil
		}
		err = cmp.Or(err, e)
	}
	return cmp.Or(err, errNoToken)
}

var (
	forwardRequest  = []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"}
	forwardResponse = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"}
)

func (h *Handler) fetch(r *http.Request, bucket, key string) (*http.Response, error) {
	u := *h.base
	u.Path = h.base.Path + "/" + bucket + "/" + key
	up, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for _, name := range forwardRequest {
		if v := r.Header.Values(name); len(v) > 0 {
			up.Header[name] = v
		}
	}
	up.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	if err := h.signer.SignHTTP(r.Context(), h.creds, up, emptySHA256, "s3", h.cfg.Region, time.Now()); err != nil {
		return nil, err
	}
	return h.cfg.Client.Do(up)
}

// relay streams the bucket's answer, as an attachment when dl is set.
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, key string, resp *http.Response, cacheControl string, dl *string, public bool) {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified,
		http.StatusPreconditionFailed, http.StatusRequestedRangeNotSatisfiable:
	case http.StatusNotFound, http.StatusForbidden:
		h.cfg.Logger.Debug("media-gateway: not found", "key", key, "status", resp.StatusCode)
		h.fail(w, http.StatusNotFound)
		return
	default:
		h.cfg.Logger.Error("media-gateway: upstream status", "key", key, "status", resp.StatusCode)
		h.fail(w, http.StatusBadGateway)
		return
	}
	hdr := w.Header()
	body := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
	for _, name := range forwardResponse {
		if v := resp.Header.Values(name); len(v) > 0 && (body || name != "Content-Length" && name != "Content-Type") {
			hdr[name] = v // error bodies are not relayed
		}
	}
	if !body && resp.StatusCode != http.StatusNotModified {
		cacheControl = "no-store" // a 412 or 416 answers this request, not the object
	}
	hdr.Set("Cache-Control", cacheControl)
	if !public {
		// Shared caches that ignore "private" (CDN rules) must not store a
		// tokened answer for the next caller.
		hdr.Set("CDN-Cache-Control", "no-store")
	}
	if dl != nil {
		hdr.Set("Content-Disposition", layout.Disposition(*dl, resp.Header.Get("Content-Type")))
	}
	securityHeaders(hdr)
	if public && (body || resp.StatusCode == http.StatusNotModified) {
		// Public renditions may be embedded by another app, without granting
		// credentialed CORS access to private files or changing denial headers.
		hdr.Set("Cross-Origin-Resource-Policy", "cross-origin")
	}
	if body && r.Method != http.MethodHead {
		// A file may outlast the server's ReadTimeout or WriteTimeout.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || !body {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		h.cfg.Logger.Warn("media-gateway: stream", "key", key, "err", err)
	}
}

func (h *Handler) cors(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Add("Vary", "Origin")
	o := r.Header.Get("Origin")
	var origins []string
	if obj, ok := h.parse(r); ok {
		origins = obj.route.Origins
	}
	if o == "" || !slices.Contains(origins, o) {
		return
	}
	hdr.Set("Access-Control-Allow-Origin", o)
	hdr.Set("Access-Control-Allow-Credentials", "true")
	if r.Method == http.MethodOptions {
		hdr.Set("Access-Control-Allow-Methods", "GET, HEAD")
		hdr.Set("Access-Control-Allow-Headers", "Range, If-None-Match, If-Modified-Since, If-Range")
		hdr.Set("Access-Control-Max-Age", "86400")
		return
	}
	hdr.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, ETag, Content-Disposition")
}

// fail writes the one response per status: no-store, so a denial never masks
// a later authorized request, and an object's security headers.
func (h *Handler) fail(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	securityHeaders(w.Header())
	http.Error(w, http.StatusText(status), status)
}

func securityHeaders(hdr http.Header) {
	hdr.Set("Cross-Origin-Resource-Policy", "same-site")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hdr.Set("Referrer-Policy", "no-referrer")
}
