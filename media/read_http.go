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
	// SlotDefault is the image a slot link redirects to while the slot has
	// none (unset, hidden or not yet encoded), e.g. "/static/avatar.svg";
	// nil or "" answers 404.
	SlotDefault func(kind, slot string) string
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
//	GET /{kind}/{id}/hls/{file}/video/{rung}-{codec}.m3u8, audio/{track}.m3u8, subs/{track}.m3u8
//	GET /{kind}/{id}/hls/{file}/sprite.vtt
//	GET /{kind}/{id}/download/{key} -> 302 to the signed download URL
//	GET /{kind}/{id}/slots/{slot} -> SlotManifest
//	GET /{kind}/{id}/slots/{slot}/image?w=320 -> 302 to the slot's public image (Reader.SlotLink)
//	GET /{kind}/{id}/video-images -> VideoImages without selections
//
// The slot image route is the stable link: it reads only the slot index and
// redirects to the narrowest public output at least w wide (the widest
// without w), else to HandlerOptions.SlotDefault, with "public, max-age=60".
// Every other request resolves the item once and is "private, no-store";
// playlists and redirects carry a folder cookie only for unversioned
// full-access items in cookie mode. Disabled generic downloads return 404
// and are omitted from read results. Signed responses log the viewer, item,
// access and expiry, plus a short hash when they issue a folder token.
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
	mux.HandleFunc("GET /{kind}/{id}/slots/{slot}/image", func(w http.ResponseWriter, req *http.Request) {
		r.serveSlotImage(w, req, o, log)
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

// serveSlotImage redirects a slot link (Reader.SlotLink) from the slot index.
func (r *Reader) serveSlotImage(w http.ResponseWriter, req *http.Request, o HandlerOptions, log *slog.Logger) {
	kind, id, slot := req.PathValue("kind"), req.PathValue("id"), req.PathValue("slot")
	width := 0
	if v := req.URL.Query().Get("w"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxSlotWidth {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid width", "code": "invalid_request"})
			return
		}
		width = n
	}
	notFound := func() {
		w.Header().Set("Cache-Control", "public, max-age=60")
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "code": "not_found"})
	}
	k, err := r.kinds.Kind(kind)
	if _, ok := k.Slots[slot]; err != nil || !ok || contentref.ValidateID(id) != nil {
		notFound()
		return
	}
	if r.slots == nil {
		log.Error("media slot link without a slot index", "path", req.URL.Path)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error", "code": "internal_error"})
		return
	}
	rows, err := r.slots.lookup(req.Context(), o.Tenant, kind, slot, []string{id})
	if err != nil {
		log.Error("media slot link failed", "path", req.URL.Path, "err", err.Error())
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error", "code": "internal_error"})
		return
	}
	target := ""
	if s, ok := rows[id]; ok {
		item, err := r.kinds.Item(contentref.New(o.Tenant, kind, id))
		if err != nil {
			notFound()
			return
		}
		if width == 0 {
			width = maxSlotWidth
		}
		target = OutputURLs{BaseURL: r.base.String()}.url(item, s.pick(width).Blob, true)
	} else if o.SlotDefault != nil {
		target = o.SlotDefault(kind, slot)
	}
	if target == "" {
		notFound()
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	http.Redirect(w, req, target, http.StatusFound)
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
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable", "media storage is unavailable"
	}
	return http.StatusInternalServerError, "internal_error", "internal error"
}
