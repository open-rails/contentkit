package contenturl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// Visibility is the host's verdict on resolved content for one request.
// The zero value is Hidden, so an unset verdict fails closed.
type Visibility int

const (
	// Hidden content answers like an unknown code (the host's 404): no
	// redirect, so nothing about it (its slug) leaks.
	Hidden Visibility = iota
	// Visible content is served and canonicalized.
	Visible
	// Gone content was removed for good: 410, no redirect.
	Gone
)

// RouterOptions configures a Router.
type RouterOptions struct {
	// Routes maps content kinds to the host's routes. Required.
	Routes Routes
	// Languages are the language segments a path may start with
	// (/{lang}/{route}/...). The prefix is kept in canonical paths and picks
	// the localized slug.
	Languages []string
	// BaseURL ("https://example.com") makes Middleware send the canonical URL
	// as a `Link: <url>; rel="canonical"` header. Empty: no header.
	BaseURL string
	// Visibility is the host's verdict on resolved content (drafts, removed
	// content, viewer restrictions). nil: every registered code is Visible.
	Visibility func(r *http.Request, link Link) (Visibility, error)
	// Gone answers requests for Gone content. nil: a plain 410.
	Gone http.Handler
	// Logger receives resolution failures (5xx). nil: slog.Default().
	Logger *slog.Logger
}

// Router maps content kinds to the host's routes over a Store.
type Router struct {
	store *Store
	opts  RouterOptions
	log   *slog.Logger
}

// NewRouter validates the options and returns the router.
func NewRouter(store *Store, opts RouterOptions) (*Router, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: Store is required", ErrInvalid)
	}
	if err := opts.Routes.Validate(opts.Languages); err != nil {
		return nil, err
	}
	opts.BaseURL = strings.TrimSuffix(opts.BaseURL, "/")
	if opts.BaseURL != "" && !strings.HasPrefix(opts.BaseURL, "https://") && !strings.HasPrefix(opts.BaseURL, "http://") {
		return nil, fmt.Errorf("%w: BaseURL %q must be an http(s) origin", ErrInvalid, opts.BaseURL)
	}
	if opts.Gone == nil {
		opts.Gone = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "gone", http.StatusGone)
		})
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Router{store: store, opts: opts, log: log}, nil
}

// Path returns link's canonical path under language ("" unprefixed); false
// when its kind has no route.
func (r *Router) Path(link Link, language string) (string, bool) {
	return Canonical(r.opts.Routes, language, link)
}

// Decision is the canonical verdict on a request.
type Decision struct {
	// Matched: the request names visible, routed content. When neither
	// Matched nor Gone, the host serves the request as it would without the
	// router (usually its 404).
	Matched bool
	// Gone: the request names routed content the host removed (410).
	Gone bool
	// Redirect: the path is not canonical; answer 301 to Location.
	Redirect bool
	Link     Link
	Language string
	Path     string // the canonical path
	Location string // Path plus the request's query
	Locator  string // DecideAlias: the alias's position inside the content
}

// Decide parses the request path, resolves its code and compares the path
// with the canonical one. A wrong route, a non-canonical code spelling, a
// merged code, a missing or stale slug and a trailing slash all redirect.
func (r *Router) Decide(req *http.Request) (Decision, error) {
	p, ok := ParsePath(req.URL.Path, r.opts.Routes, r.opts.Languages)
	if !ok {
		return Decision{}, nil
	}
	link, err := r.store.Resolve(req.Context(), p.Code)
	if errors.Is(err, ErrNotFound) {
		return Decision{}, nil
	}
	if err != nil {
		return Decision{}, err
	}
	d, err := r.decide(req, link, p.Language)
	if err != nil || !d.Matched {
		return d, err
	}
	if req.URL.RawQuery != "" {
		d.Location += "?" + req.URL.RawQuery
	}
	d.Redirect = req.URL.Path != d.Path
	return d, nil
}

func (r *Router) decide(req *http.Request, link Link, language string) (Decision, error) {
	path, ok := r.Path(link, language)
	if !ok {
		return Decision{}, nil
	}
	v := Visible
	if r.opts.Visibility != nil {
		var err error
		if v, err = r.opts.Visibility(req, link); err != nil {
			return Decision{}, err
		}
	}
	switch v {
	case Visible:
		return Decision{Matched: true, Link: link, Language: language, Path: path, Location: path}, nil
	case Gone:
		return Decision{Gone: true}, nil
	default:
		return Decision{}, nil
	}
}

// DecideAlias resolves a legacy identifier for a host's legacy redirect:
// Matched with the canonical Path (Redirect is always true), Gone, or
// neither (unknown or hidden: 404). language picks the prefix and slug.
func (r *Router) DecideAlias(req *http.Request, source, legacyKind, key, language string) (Decision, error) {
	m, err := r.store.ResolveAlias(req.Context(), source, legacyKind, key)
	if errors.Is(err, ErrNotFound) {
		return Decision{}, nil
	}
	if err != nil {
		return Decision{}, err
	}
	d, err := r.decide(req, m.Link, language)
	if d.Matched {
		d.Redirect, d.Locator = true, m.Locator
	}
	return d, err
}

type ctxKey struct{}

// FromContext returns the decision Middleware matched for this request.
func FromContext(ctx context.Context) (Decision, bool) {
	d, ok := ctx.Value(ctxKey{}).(Decision)
	return d, ok
}

// Middleware canonicalizes content page requests (GET and HEAD): a
// non-canonical path is answered 301 with the canonical path and the query
// unchanged, Gone content with RouterOptions.Gone, and a canonical path
// reaches next with the Decision in its context (FromContext) and, when
// BaseURL is set, a canonical Link header. Other requests pass through.
func (r *Router) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			next.ServeHTTP(w, req)
			return
		}
		d, err := r.Decide(req)
		switch {
		case err != nil:
			r.log.ErrorContext(req.Context(), "contenturl: resolve", "path", req.URL.Path, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		case d.Gone:
			r.opts.Gone.ServeHTTP(w, req)
		case !d.Matched:
			next.ServeHTTP(w, req)
		case d.Redirect:
			http.Redirect(w, req, d.Location, http.StatusMovedPermanently)
		default:
			if r.opts.BaseURL != "" {
				w.Header().Add("Link", "<"+r.opts.BaseURL+d.Path+`>; rel="canonical"`)
			}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxKey{}, d)))
		}
	})
}

// Resolved is Handler's response: the link and its canonical path ("" when
// the kind has no route).
type Resolved struct {
	Link
	Path string `json:"path"`
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Handler serves GET /{code}[?lang=xx]: the code's link and canonical path,
// for clients that hold only a code. Any Crockford spelling resolves; the
// response carries the canonical code. Unknown and hidden codes are 404
// not_found, Gone content 410 gone, malformed codes 400 invalid_request.
// Mount it under a prefix with http.StripPrefix.
func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{code}", func(w http.ResponseWriter, req *http.Request) {
		code, err := ParseCode(req.PathValue("code"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "not a content code", Code: "invalid_request"})
			return
		}
		lang := req.URL.Query().Get("lang")
		if lang != "" && !slices.Contains(r.opts.Languages, lang) {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown language", Code: "invalid_request"})
			return
		}
		link, err := r.store.Resolve(req.Context(), code)
		v := Visible
		if err == nil && r.opts.Visibility != nil {
			v, err = r.opts.Visibility(req, link)
		}
		switch {
		case errors.Is(err, ErrNotFound) || err == nil && v != Visible && v != Gone:
			writeJSON(w, http.StatusNotFound, errorBody{Error: "no content has this code", Code: "not_found"})
		case err != nil:
			r.log.ErrorContext(req.Context(), "contenturl: resolve", "code", code, "err", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error", Code: "internal_error"})
		case v == Gone:
			writeJSON(w, http.StatusGone, errorBody{Error: "this content was removed", Code: "gone"})
		default:
			path, _ := r.Path(link, lang)
			writeJSON(w, http.StatusOK, Resolved{Link: link, Path: path})
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
