// Package accessworker is the media access worker's HTTP handler, run by
// cmd/media-access: it checks the token for a private/ path (URL `?t=` or
// cookie `mt`), serves public/ paths without one, refuses the manifest,
// originals/ and staging/, and streams the object from the private bucket
// with its own read-only key. Every refusal (no or bad token, unservable
// area, missing object) is the same 404, so a response never reveals that
// protected content exists; token denials are decided before any bucket
// access and their reason is logged at debug. Every object
// carries Cross-Origin-Resource-Policy (default same-site), so other sites
// cannot embed it. It has no database, no host calls and no cache.
package accessworker

import (
	"cmp"
	"context"
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
	// HealthPath answers 200 without touching the bucket.
	HealthPath = "/healthz"

	// Every served name is its content's SHA-256: nothing is rewritten.
	privateCacheControl = "private, max-age=31536000, immutable"
	publicCacheControl  = "public, max-age=31536000, immutable"
)

// Config configures a Handler. The S3 key should only be able to read
// */private/* and */public/*.
type Config struct {
	Endpoint        string // path-style S3 endpoint, e.g. http://rook-ceph-rgw-external-rgw.rook-ceph.svc:7480
	Bucket          string
	Region          string // default us-east-1
	AccessKeyID     string
	SecretAccessKey string
	Ring            token.Ring
	// Hosts limits the Host header (without port); empty accepts any
	// (not recommended in production).
	Hosts []string
	// Origins are exact CORS origins ("https://example.com", no path or
	// wildcard) allowed with credentials: the sites whose hls.js reads blobs.
	Origins []string
	// ResourcePolicy is the Cross-Origin-Resource-Policy on every object:
	// "same-site" (default: only the site's own domain and subdomains may
	// embed media), "same-origin", or "cross-origin" for a deployment whose
	// pages are on another site than its media.
	ResourcePolicy string
	// Client reaches the bucket; default a transport without compression.
	Client *http.Client
	Logger *slog.Logger
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
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("accessworker: Endpoint, Bucket and S3 credentials are required")
	}
	if !layout.ValidSegment(cfg.Bucket) {
		return nil, errors.New("accessworker: invalid Bucket")
	}
	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("accessworker: invalid Endpoint")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	switch cfg.ResourcePolicy {
	case "":
		cfg.ResourcePolicy = "same-site"
	case "same-site", "same-origin", "cross-origin":
	default:
		return nil, fmt.Errorf("accessworker: invalid ResourcePolicy %q", cfg.ResourcePolicy)
	}
	for _, o := range cfg.Origins {
		if err := validOrigin(o); err != nil {
			return nil, err
		}
	}
	for _, h := range cfg.Hosts {
		if h == "" || strings.ContainsAny(h, "/:*") {
			return nil, fmt.Errorf("accessworker: invalid host %q (a bare host name)", h)
		}
	}
	cfg.Hosts = slices.Clone(cfg.Hosts)
	for i, h := range cfg.Hosts {
		cfg.Hosts[i] = strings.ToLower(h)
	}
	return &Handler{
		cfg:    cfg,
		base:   base,
		creds:  aws.Credentials{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey},
		signer: v4.NewSigner(),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if path == HealthPath {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if !h.hostAllowed(r.Host) {
		h.fail(w, http.StatusMisdirectedRequest)
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
	// Keys are library-built ASCII; anything escaped, dotted or nested is not one.
	key, ok := strings.CutPrefix(path, "/")
	k, parsed := layout.Parse(key)
	if !ok || !parsed || r.URL.RawPath != "" {
		h.fail(w, http.StatusNotFound)
		return
	}
	var disposition string
	switch k.Area {
	case layout.AreaPublic:
	case layout.AreaPrivate:
		q := r.URL.Query()
		dl := q.Get("dl")
		if err := h.authorize(r, q.Get("t"), key, dl); err != nil {
			h.cfg.Logger.Debug("media-access: denied", "key", key, "reason", err)
			h.fail(w, http.StatusNotFound)
			return
		}
		if dl != "" {
			disposition = token.Attachment(dl)
		}
	default: // the manifest, originals and staging are never served
		h.fail(w, http.StatusNotFound)
		return
	}
	h.stream(w, r, key, k, disposition)
}

var errNoToken = errors.New("no token")

// authorize accepts the URL token or any mt cookie (a browser may send
// several for nested paths or domains) that covers key; the error is the
// first rejection's reason, for logs only.
func (h *Handler) authorize(r *http.Request, t, key, dl string) error {
	now := time.Now()
	var first error
	if t != "" {
		if first = h.cfg.Ring.Verify(t, key, dl, now); first == nil {
			return nil
		}
	}
	if dl != "" { // download names are only signed into URL tokens
		return cmp.Or(first, errNoToken)
	}
	for _, c := range r.CookiesNamed(token.CookieName) {
		err := h.cfg.Ring.Verify(c.Value, key, "", now)
		if err == nil {
			return nil
		}
		first = cmp.Or(first, err)
	}
	return cmp.Or(first, errNoToken)
}

var (
	forwardRequest  = []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"}
	forwardResponse = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"}
)

func (h *Handler) stream(w http.ResponseWriter, r *http.Request, key string, k layout.Key, disposition string) {
	u := *h.base
	u.Path = h.base.Path + "/" + h.cfg.Bucket + "/" + key
	up, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), nil)
	if err != nil {
		h.fail(w, http.StatusInternalServerError)
		return
	}
	for _, name := range forwardRequest {
		if v := r.Header.Values(name); len(v) > 0 {
			up.Header[name] = v
		}
	}
	up.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	if err := h.signer.SignHTTP(r.Context(), h.creds, up, emptySHA256, "s3", h.cfg.Region, time.Now()); err != nil {
		h.fail(w, http.StatusInternalServerError)
		return
	}
	resp, err := h.cfg.Client.Do(up)
	if err != nil {
		if r.Context().Err() == nil {
			h.cfg.Logger.Error("media-access: upstream", "key", key, "err", err)
		}
		h.fail(w, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified,
		http.StatusPreconditionFailed, http.StatusRequestedRangeNotSatisfiable:
	case http.StatusNotFound, http.StatusForbidden: // 403: missing key without ListBucket
		h.cfg.Logger.Debug("media-access: not found", "key", key, "status", resp.StatusCode)
		h.fail(w, http.StatusNotFound)
		return
	default:
		h.cfg.Logger.Error("media-access: upstream status", "key", key, "status", resp.StatusCode)
		h.fail(w, http.StatusBadGateway)
		return
	}
	hdr := w.Header()
	ok := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
	for _, name := range forwardResponse {
		if !ok && (name == "Content-Length" || name == "Content-Type") {
			continue // error bodies are not relayed
		}
		if v := resp.Header.Values(name); len(v) > 0 {
			hdr[name] = v
		}
	}
	if k.Area == layout.AreaPublic {
		hdr.Set("Cache-Control", publicCacheControl)
	} else {
		hdr.Set("Cache-Control", privateCacheControl)
	}
	if disposition != "" {
		hdr.Set("Content-Disposition", disposition)
	}
	hdr.Set("Cross-Origin-Resource-Policy", h.cfg.ResourcePolicy)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || !ok {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil && !errors.Is(err, context.Canceled) {
		h.cfg.Logger.Warn("media-access: stream", "key", key, "err", err)
	}
}

// validOrigin accepts exactly "scheme://host[:port]".
func validOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Contains(u.Host, "*") ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || o != u.Scheme+"://"+u.Host {
		return fmt.Errorf("accessworker: invalid CORS origin %q (want exactly scheme://host[:port])", o)
	}
	return nil
}

func (h *Handler) hostAllowed(host string) bool {
	if len(h.cfg.Hosts) == 0 {
		return true
	}
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	return slices.Contains(h.cfg.Hosts, strings.ToLower(host))
}

func (h *Handler) cors(w http.ResponseWriter, r *http.Request) {
	if len(h.cfg.Origins) == 0 {
		return
	}
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

// fail writes the one error response per status: no-store so a denial
// never masks a later authorized request, and the same security headers as
// an object.
func (h *Handler) fail(w http.ResponseWriter, status int) {
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Cross-Origin-Resource-Policy", h.cfg.ResourcePolicy)
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.Error(w, http.StatusText(status), status)
}
