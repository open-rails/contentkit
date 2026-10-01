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

// HandlerOptions configure the read API.
type HandlerOptions struct {
	Identity Identity
	Logger   *slog.Logger
	// Limit is the per-viewer rate limit (default 2/s, burst 120, per
	// process; set Limit.Redis to share it across replicas): every read,
	// playlist and download counts.
	// Viewers are keyed by Actor.ID, anonymous ones by Actor.IP, else by the
	// connection's address.
	Limit RateLimit
}

// Handler serves the read API. The host mounts it under a prefix such as
// "/media/" after its auth middleware. Errors are JSON {"error", "code"}:
// 400 invalid_request, 404 not_found (also for what the viewer may not see),
// 429 rate_limited (Retry-After), 503 unavailable, 500 internal_error (a
// resolver error denies this way).
//
//	GET /{kind}/{id}?prefix=low-res/&offset=0&limit=50&download&editor -> ReadResult (+ Set-Cookie mt)
//	GET /{kind}/{id}/hls/{dir}master.m3u8?audio=ja&subs=en (filters optional; empty = none)
//	GET /{kind}/{id}/hls/{path}.m3u8   a track's media playlist
//	GET /{kind}/{id}/hls/{dir}sprite.vtt
//
// Every response is "private, no-store"; playlists carry the item cookie
// like reads. Signed responses log the viewer, item, access and expiry.
func (r *Reader) Handler(o HandlerOptions) http.Handler {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{kind}/{id}", func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		w.Header().Set("Cache-Control", "private, no-store")
		res, g, err := r.serveRead(req, o)
		if err != nil {
			fail(w, req, log, err)
			return
		}
		if res.Cookie != nil {
			http.SetCookie(w, res.Cookie)
		}
		writeJSON(w, http.StatusOK, res)
		g.logIssued(req, log)
		log.Debug("media read", "path", req.URL.Path, "duration", time.Since(start))
	})
	mux.HandleFunc("GET /{kind}/{id}/hls/{path...}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		ref, actor, err := r.requestRef(req, o)
		var g *Grant
		if err == nil {
			g, err = r.Grant(req.Context(), ref, actor)
		}
		var body []byte
		contentType := HLSContentType
		if err == nil {
			p := req.PathValue("path")
			switch {
			case strings.HasSuffix(p, "/master.m3u8") || p == "master.m3u8":
				q := req.URL.Query()
				body, err = g.MasterPlaylist(strings.TrimSuffix(p, "master.m3u8"), MasterOptions{Audio: queryList(q, "audio"), Subs: queryList(q, "subs")})
			case strings.HasSuffix(p, "/sprite.vtt") || p == "sprite.vtt":
				body, err = g.SpriteVTT(req.Context(), strings.TrimSuffix(p, "sprite.vtt"))
				contentType = VTTContentType
			case strings.HasSuffix(p, ".m3u8"):
				body, err = g.MediaPlaylist(req.Context(), strings.TrimSuffix(p, ".m3u8"))
			default:
				err = ErrNotAllowed
			}
		}
		if err != nil {
			fail(w, req, log, err)
			return
		}
		if c := g.Cookie(); c != nil {
			http.SetCookie(w, c)
		}
		g.logIssued(req, log)
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	})
	return limited(mux, o, log)
}

func (r *Reader) serveRead(req *http.Request, o HandlerOptions) (*ReadResult, *Grant, error) {
	ref, actor, err := r.requestRef(req, o)
	if err != nil {
		return nil, nil, err
	}
	q := req.URL.Query()
	opts := ReadOptions{Prefix: q.Get("prefix"), Download: q.Has("download"), Editor: q.Has("editor")}
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

func (r *Reader) requestRef(req *http.Request, o HandlerOptions) (contentref.ContentRef, access.Actor, error) {
	actor := actorOf(req, o)
	ref, err := r.reg.Ref(req.PathValue("kind"), req.PathValue("id"))
	if err != nil {
		return ref, actor, ErrNotVisible
	}
	return ref, actor, nil
}

func actorOf(req *http.Request, o HandlerOptions) access.Actor {
	if o.Identity != nil {
		if a, ok := o.Identity.Actor(req.Context()); ok {
			return a
		}
	}
	return access.Actor{Anonymous: true}
}

func fail(w http.ResponseWriter, req *http.Request, log *slog.Logger, err error) {
	status, code, msg := http.StatusInternalServerError, "internal_error", "internal error"
	switch {
	case errors.Is(err, ErrNotVisible), errors.Is(err, ErrNotAllowed):
		status, code, msg = http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, ErrInvalidRequest):
		status, code, msg = http.StatusBadRequest, "invalid_request", "invalid request"
	case errors.Is(err, ErrUnavailable):
		status, code, msg = http.StatusServiceUnavailable, "unavailable", "media storage is unavailable"
	default:
		log.Error("media read failed", "path", req.URL.Path, "err", err.Error())
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// limited applies HandlerOptions.Limit to every route.
func limited(next http.Handler, o HandlerOptions, log *slog.Logger) http.Handler {
	lim := newRateLimiter(o.Limit, viewerLimit, "read", time.Now, log)
	if lim == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor := actorOf(req, o)
		if ok, wait := lim.allow(req.Context(), viewerKey(req, actor)); !ok {
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
	access := AccessNone
	switch {
	case g.Full():
		access = AccessFull
	case g.units > 0:
		access = AccessPreview
	}
	attrs := []any{"viewer", g.actor.ID, "anonymous", g.actor.Anonymous, "ref", g.Item.Ref().String(),
		"path", req.URL.Path, "access", access, "editor", g.Editor(), "expires", g.Expires.Unix()}
	if g.item != "" {
		sum := sha256.Sum256([]byte(g.item))
		attrs = append(attrs, "token", hex.EncodeToString(sum[:6]))
	}
	log.Info("media urls signed", attrs...)
}

// queryList is a comma-separated query filter; nil when the parameter is absent.
func queryList(q map[string][]string, name string) []string {
	vs, ok := q[name]
	if !ok {
		return nil
	}
	out := []string{}
	for _, v := range vs {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
