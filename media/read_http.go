package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// Identity reads the authenticated actor the host's middleware put in the
// context; the same shape as content.Identity.
type Identity interface {
	Actor(ctx context.Context) (access.Actor, bool)
}

// HandlerOptions scope the read API to one tenant.
type HandlerOptions struct {
	Tenant   string
	Identity Identity
	Logger   *slog.Logger
	// Limit is the per-viewer rate limit (default ViewerLimit{}: 2/s, burst
	// 120, per process; set Limit.Redis to share it across replicas).
	// Viewers are keyed by tenant and Actor.ID, anonymous ones by Actor.IP,
	// else by the connection's address: behind a proxy, set Actor.IP from
	// the client address the proxy forwards.
	Limit ViewerLimit
}

// Handler serves the read API. The host mounts it under a prefix such as
// "/media/" after its auth middleware. {id} is "{content_id}" or
// "{content_id}@{version_id}" for a versioned kind. Errors are JSON
// {"error", "code"}: 400 invalid_request, 404 not_found (also for hidden
// items), 429 rate_limited (Retry-After; HandlerOptions.Limit), 500
// internal_error (a resolver error denies this way).
//
//	GET /{kind}/{id}?variant=high,thumb&offset=0&limit=50 -> ReadResult (+ Set-Cookie mt)
//	GET /{kind}/{id}/hls/{file}/master.m3u8?audio=ja&subs=en,s2 (filters optional; empty = none)
//	GET /{kind}/{id}/hls/{file}/video/{height}.m3u8, audio/{track}.m3u8, subs/{track}.m3u8
//	GET /{kind}/{id}/hls/{file}/sprite.vtt
//	GET /{kind}/{id}/download/{key} -> 302 to the signed download URL
//	GET /{kind}/{id}/slots/{slot} -> SlotManifest
//	GET /{kind}/{id}/video-images -> VideoImages without selections
//
// Every request resolves the item once and is "private, no-store";
// playlists and redirects carry the folder cookie in cookie mode. Each
// request that signs URLs logs the viewer, item, access and expiry, and a
// short hash of a folder token, so a leaked URL can be traced to its viewer.
func (r *Reader) Handler(o HandlerOptions) http.Handler {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	r.hlsRoutes(mux, o, log)
	mux.HandleFunc("GET /{kind}/{id}/slots/{slot}", func(w http.ResponseWriter, req *http.Request) {
		ref, actor := requestRef(req, o)
		m, err := r.Slot(req.Context(), ref, actor, req.PathValue("slot"))
		if err != nil {
			status, code, msg := classify(err)
			if status >= http.StatusInternalServerError {
				log.Error("media slot read failed", "path", req.URL.Path, "err", err.Error())
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, status, map[string]string{"error": msg, "code": code})
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("GET /{kind}/{id}/video-images", func(w http.ResponseWriter, req *http.Request) {
		ref, actor := requestRef(req, o)
		v, err := r.VideoImages(req.Context(), ref, actor)
		if err != nil {
			status, code, msg := classify(err)
			if status >= http.StatusInternalServerError {
				log.Error("media video images read failed", "path", req.URL.Path, "err", err.Error())
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, status, map[string]string{"error": msg, "code": code})
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("GET /{kind}/{id}", func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		res, g, err := r.serveRead(req, o)
		status := http.StatusOK
		w.Header().Set("Cache-Control", "private, no-store")
		if err != nil {
			var code, msg string
			status, code, msg = classify(err)
			if status >= http.StatusInternalServerError {
				log.Error("media read failed", "path", req.URL.Path, "err", err.Error())
			}
			writeJSON(w, status, map[string]string{"error": msg, "code": code})
		} else {
			if res.Cookie != nil {
				http.SetCookie(w, res.Cookie)
			}
			writeJSON(w, status, res)
			g.logIssued(req, log)
		}
		log.Debug("media read", "path", req.URL.Path, "status", status, "duration", time.Since(start))
	})
	return limited(mux, o, log)
}

// limited applies HandlerOptions.Limit to every route.
func limited(next http.Handler, o HandlerOptions, log *slog.Logger) http.Handler {
	lim := newViewerLimiter(o.Limit, time.Now, log)
	if lim == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, actor := requestRef(req, o)
		key := o.Tenant + "|" + viewerKey(req, actor)
		if ok, wait := lim.allow(req.Context(), key); !ok {
			log.Warn("media read rate limited", "viewer", actor.ID, "anonymous", actor.Anonymous, "path", req.URL.Path)
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests", "code": "rate_limited"})
			return
		}
		next.ServeHTTP(w, req)
	})
}

// viewerKey is the rate-limit key: the actor, else its IP, else the peer address.
func viewerKey(req *http.Request, a access.Actor) string {
	switch {
	case !a.Anonymous && a.ID != "":
		return "a:" + a.ID
	case a.IP != "":
		return "ip:" + a.IP
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	return "ip:" + host
}

// logIssued records the URLs a grant signed for its viewer.
func (g *Grant) logIssued(req *http.Request, log *slog.Logger) {
	if g == nil {
		return
	}
	access := AccessNone
	switch {
	case g.Full():
		access = AccessFull
	case g.units > 0:
		access = AccessPreview
	}
	attrs := []any{"viewer", g.actor.ID, "anonymous", g.actor.Anonymous, "ref", g.Item.Ref().String(),
		"path", req.URL.Path, "access", access, "editor", g.Editor(), "expires", g.Expires.Unix()}
	if g.folder != "" {
		sum := sha256.Sum256([]byte(g.folder))
		attrs = append(attrs, "token", hex.EncodeToString(sum[:6]))
	}
	log.Info("media urls signed", attrs...)
}

func (r *Reader) serveRead(req *http.Request, o HandlerOptions) (*ReadResult, *Grant, error) {
	ref, actor := requestRef(req, o)
	q := req.URL.Query()
	var opts ReadOptions
	for _, v := range q["variant"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				opts.Variants = append(opts.Variants, name)
			}
		}
	}
	for _, p := range []struct {
		name string
		dst  *int
	}{{"offset", &opts.Offset}, {"limit", &opts.Limit}} {
		if s := q.Get(p.name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return nil, nil, ErrInvalidRequest
			}
			*p.dst = n
		}
	}
	return r.read(req.Context(), ref, actor, opts)
}

func requestRef(req *http.Request, o HandlerOptions) (contentref.ContentRef, access.Actor) {
	id, version, _ := strings.Cut(req.PathValue("id"), "@")
	actor := access.Actor{Anonymous: true}
	if o.Identity != nil {
		if a, ok := o.Identity.Actor(req.Context()); ok {
			actor = a
		}
	}
	return contentref.New(o.Tenant, req.PathValue("kind"), id).WithVersion(version), actor
}

func classify(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrNotVisible), errors.Is(err, ErrNotAllowed):
		return http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request", "invalid request"
	}
	return http.StatusInternalServerError, "internal_error", "internal error"
}
