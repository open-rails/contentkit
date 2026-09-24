package media

import (
	"context"
	"errors"
	"log/slog"
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
}

// Handler serves the read API. The host mounts it under a prefix such as
// "/media/" after its auth middleware. {id} is "{content_id}" or
// "{content_id}@{version_id}" for a versioned kind. Errors are JSON
// {"error", "code"}: 400 invalid_request, 404 not_found (also for hidden
// items), 500 internal_error (a resolver error denies this way).
//
//	GET /{kind}/{id}?variant=high,thumb&offset=0&limit=50 -> ReadResult (+ Set-Cookie mt)
//	GET /{kind}/{id}/hls/{file}/master.m3u8?audio=ja&subs=en,s2 (filters optional; empty = none)
//	GET /{kind}/{id}/hls/{file}/video/{height}.m3u8, audio/{track}.m3u8, subs/{track}.m3u8
//	GET /{kind}/{id}/hls/{file}/sprite.vtt
//	GET /{kind}/{id}/download/{key} -> 302 to the signed download URL
//	GET /{kind}/{id}/slots/{slot} -> SlotManifest (public; no resolve, no-cache)
//
// Every request resolves the item once; playlists and redirects are
// "private, no-store" and carry the folder cookie in cookie mode.
func (r *Reader) Handler(o HandlerOptions) http.Handler {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	r.hlsRoutes(mux, o, log)
	mux.HandleFunc("GET /{kind}/{id}/slots/{slot}", func(w http.ResponseWriter, req *http.Request) {
		ref, _ := requestRef(req, o)
		m, err := r.Slot(req.Context(), ref, req.PathValue("slot"))
		if err != nil {
			status, code, msg := classify(err)
			if status >= http.StatusInternalServerError {
				log.Error("media slot read failed", "path", req.URL.Path, "err", err.Error())
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, status, map[string]string{"error": msg, "code": code})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("GET /{kind}/{id}", func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		res, err := r.serveRead(req, o)
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
		}
		log.Debug("media read", "path", req.URL.Path, "status", status, "duration", time.Since(start))
	})
	return mux
}

func (r *Reader) serveRead(req *http.Request, o HandlerOptions) (*ReadResult, error) {
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
				return nil, ErrInvalidRequest
			}
			*p.dst = n
		}
	}
	return r.Read(req.Context(), ref, actor, opts)
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
