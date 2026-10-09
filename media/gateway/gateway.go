// Package gateway is the media gateway run by cmd/media-gateway. For the
// namespaces served on the request's host it streams, from a private bucket
// with its own read-only key:
//
//	/v1/{ns}/{kind}/{id}/public/{name}         to anyone; when missing, {ns}/{kind}/_default/public/{name}
//	                                           if a Default declares the name
//	/v1/{ns}/{kind}/{id}/private/sha256-{hex}  with the item's unexpired token (?t= or an mt cookie);
//	                                           ?dl={name} serves it as a download under that name
//
// A token opens every private file of its item or none. Everything else and
// every denial is one identical no-store 404; token denials are decided
// before any bucket access. It keeps no state, reads no manifests and no
// databases, and knows nothing of apps, presets or originals.
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
	"regexp"
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
type Config struct {
	Endpoint, Bucket, Region     string // path-style S3 endpoint; Region default us-east-1
	AccessKeyID, SecretAccessKey string // a key that can read only */private/* and */public/*
	Ring                         token.Ring
	Hosts                        map[string][]string // lower-case host name (no port) -> namespaces served on it; required
	Origins                      []string            // exact CORS origins ("scheme://host[:port]") allowed with credentials
	Defaults                     []layout.Default    // public default images
	Client                       *http.Client        // default: a tuned transport without compression
	Logger                       *slog.Logger
}

// Handler serves media objects.
type Handler struct {
	cfg      Config
	base     *url.URL
	creds    aws.Credentials
	signer   *v4.Signer
	defaults map[string][]*regexp.Regexp // "{ns}/{kind}" -> name templates
}

var emptySHA256 = hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }())

// New validates cfg.
func New(cfg Config) (*Handler, error) {
	if cfg.Endpoint == "" || !layout.ValidSegment(cfg.Bucket) || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("gateway: Endpoint, a valid Bucket and S3 credentials are required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("gateway: invalid Endpoint")
	}
	if cfg.Hosts, err = layout.CheckHosts(cfg.Hosts); err != nil {
		return nil, err
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
	key := o.key(o.id)
	cacheControl := publicCacheControl
	var dl *string // a private file's unsigned download name
	if o.area == "private" {
		if err := h.authorize(r, key); err != nil {
			h.cfg.Logger.Debug("media-gateway: denied", "key", key, "reason", err)
			h.fail(w, http.StatusNotFound)
			return
		}
		cacheControl = privateCacheControl
		if q := r.URL.Query(); q.Has("dl") {
			dl = new(q.Get("dl"))
		}
	}
	resp, err := h.fetch(r, key)
	if err == nil && missing(resp.StatusCode) && o.area == "public" && h.hasDefault(o) {
		_ = resp.Body.Close()
		key = o.key(layout.DefaultID)
		resp, err = h.fetch(r, key)
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

type object struct{ ns, kind, id, area, name string }

func (o object) key(id string) string {
	return o.ns + "/" + o.kind + "/" + id + "/" + o.area + "/" + o.name
}

// parse accepts exactly /v1/{ns}/{kind}/{id}/{public|private}/{name},
// unescaped, for a namespace served on the request's host.
func (h *Handler) parse(r *http.Request) (object, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, layout.URLPrefix)
	p := strings.Split(rest, "/")
	if !ok || r.URL.RawPath != "" || len(p) != 5 || slices.ContainsFunc(p[:4], func(s string) bool { return !layout.ValidSegment(s) }) {
		return object{}, false
	}
	o := object{p[0], p[1], p[2], p[3], p[4]}
	host := r.Host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	if strings.HasPrefix(o.id, "_") || !slices.Contains(h.cfg.Hosts[strings.ToLower(host)], o.ns) {
		return object{}, false
	}
	return o, o.area == "public" && layout.ValidPublicName(o.name) || o.area == "private" && layout.ValidHashName(o.name)
}

var errNoToken = errors.New("no token")

// authorize accepts the URL token or any mt cookie (a browser may send
// several) of key's item.
func (h *Handler) authorize(r *http.Request, key string) (err error) {
	now := time.Now()
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

func (h *Handler) hasDefault(o object) bool {
	return slices.ContainsFunc(h.defaults[o.ns+"/"+o.kind], func(re *regexp.Regexp) bool { return re.MatchString(o.name) })
}

// missing is a bucket's answer for an absent key: 404, or 403 without ListBucket.
func missing(status int) bool { return status == http.StatusNotFound || status == http.StatusForbidden }

var (
	forwardRequest  = []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"}
	forwardResponse = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"}
)

func (h *Handler) fetch(r *http.Request, key string) (*http.Response, error) {
	u := *h.base
	u.Path = h.base.Path + "/" + h.cfg.Bucket + "/" + key
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
	if o == "" || !slices.Contains(h.cfg.Origins, o) {
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
