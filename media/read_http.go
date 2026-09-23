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
func (r *Reader) Handler(o HandlerOptions) http.Handler {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
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
	id, version, _ := strings.Cut(req.PathValue("id"), "@")
	ref := contentref.New(o.Tenant, req.PathValue("kind"), id).WithVersion(version)
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
	actor := access.Actor{Anonymous: true}
	if o.Identity != nil {
		if a, ok := o.Identity.Actor(req.Context()); ok {
			actor = a
		}
	}
	return r.Read(req.Context(), ref, actor, opts)
}

func classify(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrNotVisible):
		return http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request", "invalid request"
	}
	return http.StatusInternalServerError, "internal_error", "internal error"
}
