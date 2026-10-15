// Package gateway serves the exact app/account roots declared for a media
// host. Scoped URLs are /v1/tenant/{tenant}/app/{app}/{kind}/{id}[/{version}]/…
// or /v1/tenant/{tenant}/account/{kind}/{account}/… . Legacy namespace URLs
// remain available only through explicitly configured Hosts/Buckets.
//
// Public renditions are anonymous; private files require the item's token and
// download files require a token for that exact key. Denials use a uniform
// no-store 404 and token denials precede S3 access. Before serving a scoped
// item, a HEAD of its reserved public erasure marker rejects permanently erased
// items, including late uploads reached through old tokens. Forbidden marker
// reads fail closed. The gateway reads no manifests or databases.
package gateway

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	publicCacheControl  = "public, max-age=300, stale-while-revalidate=86400"
	privateCacheControl = "private, max-age=31536000, immutable"
)

// Config configures a Handler.
// Route exposes exactly one tenant account or app root on a media host.
// Legacy Hosts/Buckets never authorize a scoped path.
type Route struct {
	Host    string       `json:"host"`
	Scope   layout.Scope `json:"scope"`
	Bucket  string       `json:"bucket"`
	Origins []string     `json:"origins,omitempty"`
}

type Config struct {
	Routes []Route

	Endpoint, Region             string // path-style S3 endpoint; Region default us-east-1
	AccessKeyID, SecretAccessKey string // read key for served private/download/public objects, including erasure-marker HEAD; scoped prefixes need missing-object 404s
	Ring                         token.Ring
	Hosts                        map[string][]string // legacy lower-case host -> namespaces; Routes selects scoped paths
	Buckets                      map[string]string   // legacy namespace -> bucket; required for each legacy host namespace
	Origins                      []string            // exact CORS origins ("scheme://host[:port]") allowed with credentials
	Defaults                     []layout.Default    // deployment selection from image.PublishDefaults
	Client                       *http.Client        // default: a tuned transport without compression
	Logger                       *slog.Logger
}

// Handler serves media objects.
type Handler struct {
	cfg      Config
	base     *url.URL
	creds    aws.Credentials
	signer   *v4.Signer
	defaults map[string]map[string]string // "{ns}/{kind}" -> logical name -> immutable object
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
	if len(cfg.Hosts) > 0 {
		if cfg.Hosts, err = layout.CheckHosts(cfg.Hosts); err != nil {
			return nil, err
		}
		if cfg.Buckets, err = layout.CheckBuckets(cfg.Buckets); err != nil {
			return nil, err
		}
	}
	if len(cfg.Hosts) == 0 && len(cfg.Routes) == 0 {
		return nil, errors.New("gateway: at least one scoped Route or legacy Host is required")
	}
	for host, nss := range cfg.Hosts {
		for _, ns := range nss {
			if cfg.Buckets[ns] == "" {
				return nil, fmt.Errorf("gateway: host %q serves namespace %q, which has no bucket", host, ns)
			}
		}
	}
	cfg.Routes = slices.Clone(cfg.Routes)
	for i := range cfg.Routes {
		cfg.Routes[i].Origins = slices.Clone(cfg.Routes[i].Origins)
	}
	seen := map[string]bool{}
	for _, r := range cfg.Routes {
		key := r.Host + "/" + r.Scope.Partition()
		if !r.Scope.Valid() || r.Host == "" || strings.Trim(r.Host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" || !layout.ValidSegment(r.Bucket) || seen[key] {
			return nil, errors.New("gateway: invalid or duplicate scoped Route")
		}
		seen[key] = true
		for _, origin := range r.Origins {
			if err := validOrigin(origin); err != nil {
				return nil, err
			}
		}
	}
	defaults, err := layout.CompileDefaults(cfg.Defaults)
	if err != nil {
		return nil, err
	}
	for _, o := range cfg.Origins {
		if err := validOrigin(o); err != nil {
			return nil, err
		}
	}
	cfg.Origins = slices.Clone(cfg.Origins)
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
		cfg:      cfg,
		base:     base,
		creds:    aws.Credentials{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey},
		signer:   v4.NewSigner(),
		defaults: defaults,
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
		if err := h.authorize(r, o.area, key); err != nil {
			h.cfg.Logger.Debug("media-gateway: denied", "key", key, "reason", err)
			h.fail(w, http.StatusNotFound)
			return
		}
		cacheControl = privateCacheControl
		if q := r.URL.Query(); q.Has("dl") {
			dl = new(q.Get("dl"))
		}
	}
	if !o.scope.IsZero() {
		check := r.Clone(r.Context())
		check.Method = http.MethodHead
		check.Header = make(http.Header)
		fence := o
		fence.area = layout.AreaPublic
		fence.name = layout.ErasureFenceName
		status, err := h.fetch(check, o.bucket, fence.key())
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
	}
	resp, err := h.fetch(r, o.bucket, key)
	if err == nil && missing(resp.StatusCode) && o.area == "public" {
		if name := h.defaultName(o); name != "" {
			_ = resp.Body.Close()
			o.name, o.id, o.version = name, layout.DefaultID, ""
			key = o.key()
			resp, err = h.fetch(r, o.bucket, key)
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			h.cfg.Logger.Error("media-gateway: upstream", "key", key, "err", err)
		}
		h.fail(w, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	h.relay(w, r, key, resp, cacheControl, dl)
}

type object struct {
	ns, kind, id, version, area, name string
	scope                             layout.Scope
	bucket                            string
}

func (o object) key() string {
	if !o.scope.IsZero() {
		return o.scope.Prefix(o.kind, o.id, o.version) + o.area + "/" + o.name
	}
	return layout.Prefix(o.ns, o.kind, o.id, o.version) + o.area + "/" + o.name
}

// parse accepts exactly /v1/{ns}/{kind}/{id}[/{version}]/{public|private}/{name},
// unescaped, for a namespace served on the request's host.
func (h *Handler) parse(r *http.Request) (object, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, layout.URLPrefix)
	if !ok || r.URL.RawPath != "" {
		return object{}, false
	}
	k, ok := layout.Parse(rest)
	if !ok || strings.HasPrefix(k.ID, "_") {
		return object{}, false
	}
	o := object{ns: k.Namespace, kind: k.Kind, id: k.ID, version: k.Version, area: k.Area, name: k.Name, scope: k.Scope}
	if o.area != layout.AreaPublic && o.area != layout.AreaPrivate && o.area != layout.AreaDownload {
		return object{}, false
	}
	host := r.Host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.ToLower(host)
	if k.Scope.IsZero() {
		if !slices.Contains(h.cfg.Hosts[host], k.Namespace) {
			return object{}, false
		}
		o.bucket = h.cfg.Buckets[k.Namespace]
	} else {
		for _, route := range h.cfg.Routes {
			if route.Host == host && route.Scope == k.Scope {
				o.bucket = route.Bucket
				break
			}
		}
		if o.bucket == "" {
			return object{}, false
		}
	}
	return o, true
}

var errNoToken = errors.New("no token")

// authorize accepts, for a private key, the URL token or any mt cookie (a
// browser may send several) of its item; for a download key only the URL's
// download token of that key.
func (h *Handler) authorize(r *http.Request, area, key string) (err error) {
	now := time.Now()
	if area == layout.AreaDownload {
		t := r.URL.Query().Get("t")
		if t == "" {
			return errNoToken
		}
		return h.cfg.Ring.VerifyDownload(t, key, now)
	}
	var toks []string
	if t := r.URL.Query().Get("t"); t != "" {
		toks = append(toks, t)
	}
	for _, c := range r.CookiesNamed(token.CookieName) {
		toks = append(toks, c.Value)
	}
	for _, tok := range toks {
		e := h.cfg.Ring.VerifyPrivate(tok, key, now)
		if e == nil {
			return nil
		}
		err = cmp.Or(err, e)
	}
	return cmp.Or(err, errNoToken)
}

func (h *Handler) defaultName(o object) string {
	files := h.defaults[o.ns+"/"+o.kind]
	if target := files[o.name]; target != "" {
		return target
	}
	// A retired publication still has the logical name before its UUID.
	stem, ext := o.name, ""
	if i := strings.LastIndexByte(stem, '.'); i >= 0 {
		stem, ext = stem[:i], stem[i:]
	}
	if len(stem) > 37 && stem[len(stem)-37] == '-' && layout.ValidStagedName(layout.StagedPrefix+stem[len(stem)-36:]) {
		return files[stem[:len(stem)-37]+ext]
	}
	return ""
}

// missing is a bucket's answer for an absent key: 404, or 403 without ListBucket.
func missing(status int) bool { return status == http.StatusNotFound || status == http.StatusForbidden }

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
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, key string, resp *http.Response, cacheControl string, dl *string) {
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
	hdr.Set("Cache-Control", cacheControl)
	if dl != nil {
		hdr.Set("Content-Disposition", layout.Disposition(*dl, resp.Header.Get("Content-Type")))
	}
	securityHeaders(hdr)
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
	origins := h.cfg.Origins
	if obj, ok := h.parse(r); ok && !obj.scope.IsZero() {
		origins = nil
		for _, route := range h.cfg.Routes {
			host := r.Host
			if hh, _, err := net.SplitHostPort(host); err == nil {
				host = hh
			}
			if route.Host == strings.ToLower(host) && route.Scope == obj.scope && route.Bucket == obj.bucket {
				origins = append(origins, route.Origins...)
			}
		}
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
}
