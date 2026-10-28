package urlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	contenturl "github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Router maps content kinds to the host's routes over a Store.
type Router struct {
	store *Store
	opts  contenturl.RouterOptions
	log   *slog.Logger
}

// NewRouter validates the options and returns the router.
func NewRouter(store *Store, opts contenturl.RouterOptions) (*Router, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: Store is required", contenturl.ErrInvalid)
	}
	if len(opts.Languages) == 0 {
		return nil, fmt.Errorf("%w: Languages is required", contenturl.ErrInvalid)
	}
	if err := opts.Routes.Validate(opts.Languages); err != nil {
		return nil, err
	}
	opts.BaseURL = strings.TrimSuffix(opts.BaseURL, "/")
	if opts.BaseURL != "" && !strings.HasPrefix(opts.BaseURL, "https://") && !strings.HasPrefix(opts.BaseURL, "http://") {
		return nil, fmt.Errorf("%w: BaseURL %q must be an http(s) origin", contenturl.ErrInvalid, opts.BaseURL)
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

// Path returns link's canonical path in language (the default when ""), with
// its slug there; false when its kind has no route.
func (r *Router) Path(link contenturl.Link, language string) (string, bool) {
	return Canonical(r.opts.Routes, r.opts.Languages, language, link)
}

// Decide parses the request path, resolves its code and compares the path
// with the canonical one. A missing language prefix, a wrong route, a
// non-canonical code spelling, a merged code, a missing or stale slug and a
// trailing slash all redirect; the query (?v= included) is kept.
func (r *Router) Decide(req *http.Request) (contenturl.Decision, error) {
	p, ok := contenturl.ParsePath(req.URL.Path, r.opts.Routes, r.opts.Languages)
	if !ok {
		return contenturl.Decision{}, nil
	}
	links, err := r.store.Resolve(req.Context(), []contenturl.Code{p.Code})
	if err != nil {
		return contenturl.Decision{}, err
	}
	link, ok := links[p.Code]
	if !ok {
		return contenturl.Decision{}, nil
	}
	language := p.Language
	if language == "" {
		language = Prefix(r.opts.Languages, contenturl.AcceptLanguage(r.opts.Languages, req.Header.Get("Accept-Language")))
	}
	d, err := r.decide(req, link, language)
	if err != nil || !d.Matched {
		return d, err
	}
	if req.URL.RawQuery != "" {
		d.Location += "?" + req.URL.RawQuery
	}
	d.Redirect = req.URL.Path != d.Path
	return d, nil
}

func (r *Router) decide(req *http.Request, link contenturl.Link, language string) (contenturl.Decision, error) {
	path, ok := r.Path(link, language)
	if !ok {
		return contenturl.Decision{}, nil
	}
	v, err := r.visibility(req, []contenturl.Link{link})
	if err != nil {
		return contenturl.Decision{}, err
	}
	switch v[0] {
	case contenturl.Visible, contenturl.Preview:
		return contenturl.Decision{Matched: true, Private: v[0] == contenturl.Preview, Link: link, Language: language, Path: path, Location: path}, nil
	case contenturl.Gone:
		return contenturl.Decision{Gone: true}, nil
	default:
		return contenturl.Decision{}, nil
	}
}

// visibility is the host's verdict on each link.
func (r *Router) visibility(req *http.Request, links []contenturl.Link) ([]contenturl.Visibility, error) {
	if r.opts.Visibility == nil || len(links) == 0 {
		out := make([]contenturl.Visibility, len(links))
		for i := range out {
			out[i] = contenturl.Visible
		}
		return out, nil
	}
	v, err := r.opts.Visibility(req, links)
	if err == nil && len(v) != len(links) {
		err = fmt.Errorf("contenturl: Visibility answered %d verdicts for %d links", len(v), len(links))
	}
	return v, err
}

type ctxKey struct{}

// FromContext returns the decision Middleware matched for this request.
func FromContext(ctx context.Context) (contenturl.Decision, bool) {
	d, ok := ctx.Value(ctxKey{}).(contenturl.Decision)
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
			if d.Private {
				private(w)
			}
			http.Redirect(w, req, d.Location, http.StatusMovedPermanently)
		default:
			if d.Private {
				private(w)
			}
			if r.opts.BaseURL != "" {
				w.Header().Add("Link", "<"+r.opts.BaseURL+d.Path+`>; rel="canonical"`)
			}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxKey{}, d)))
		}
	})
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// resolveAliases resolves legacy identifiers for req's visitor, as the
// codes route resolves a code: hidden content is not_found, removed content
// gone. language picks the paths' prefix and slug.
func (r *Router) resolveAliases(req *http.Request, keys []contenturl.AliasKey, language string) (contenturl.AliasResults, error) {
	matches, err := r.store.ResolveAliases(req.Context(), keys)
	if err != nil {
		return contenturl.AliasResults{}, err
	}
	var links []contenturl.Link
	for _, m := range matches {
		if m != nil {
			links = append(links, m.Link)
		}
	}
	verdicts, err := r.visibility(req, links)
	if err != nil {
		return contenturl.AliasResults{}, err
	}
	out := contenturl.AliasResults{Aliases: make([]contenturl.AliasResult, len(keys))}
	n := 0
	for i, m := range matches {
		res := contenturl.AliasResult{AliasKey: keys[i], Status: contenturl.AliasNotFound}
		if m != nil {
			switch verdicts[n] {
			case contenturl.Visible:
				path, _ := r.Path(m.Link, language)
				res.Status, res.Resolved, res.Locator = contenturl.AliasFound, &contenturl.Resolved{Link: m.Link, Path: path}, m.Locator
			case contenturl.Gone:
				res.Status = contenturl.AliasGone
			}
			n++
		}
		out.Aliases[i] = res
	}
	return out, nil
}

// Handler serves GET /{code}[?lang=xx]: the code's link and canonical path,
// for clients that hold only a code. Any Crockford spelling resolves; the
// response carries the canonical code. Unknown and hidden codes are 404
// not_found, Gone content 410 gone, malformed codes 400 invalid_request.
// POST /aliases resolves legacy identifiers the same way, in batches. The
// engine serves them as the codes route group (/v1/codes).
//
// guard binds each request's actor (the code lookup is public; an editor's
// draft resolves only for them).
func (r *Router) Handler(guard httpapi.Guard) http.Handler {
	mux := http.NewServeMux()
	httpapi.Mount(mux, guard, r, codeRoutes)
	return mux
}

func init() { httpapi.Register(httpapi.Codes, codeRoutes) }

var codeRoutes = []httpapi.Route[*Router]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{code}", Resource: "codes", Auth: httpapi.Public,
		Doc: "A content code's link and canonical path; any spelling of the code resolves. With v, the live version that code names, if any. An editor of content not live yet gets it privately (no-store, noindex).",
		Query: []httpapi.Param{httpapi.Text("lang", "the language of the path's prefix and slug; the site's default by default"),
			httpapi.Text("v", "a version code (the page's ?v=)")},
		Responses: []httpapi.Reply{httpapi.OK(contenturl.Resolved{})}, Errors: []string{"gone", httpapi.CodeNotFound}},
		Serve: httpapi.H((*Router).resolve)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/aliases", Resource: "codes", Auth: httpapi.Open,
		Doc: "Resolves legacy identifiers (the import's aliases) to their content's link, canonical path and locator, in order, following merges: " +
			"found, not_found (unknown or hidden) or gone; at most 100.",
		Query:   []httpapi.Param{httpapi.Text("lang", "the language of the paths' prefix and slug; the site's default by default")},
		Request: contenturl.AliasLookup{}, Responses: []httpapi.Reply{httpapi.OK(contenturl.AliasResults{})}},
		Serve: httpapi.H((*Router).aliases)},
}

func (r *Router) resolve(w http.ResponseWriter, req *http.Request) {
	code, err := contenturl.ParseCode(req.PathValue("code"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "not a content code", Code: "invalid_request"})
		return
	}
	lang := req.URL.Query().Get("lang")
	if lang != "" && !slices.Contains(r.opts.Languages, lang) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown language", Code: "invalid_request"})
		return
	}
	links, err := r.store.Resolve(req.Context(), []contenturl.Code{code})
	link, found := links[code]
	v := contenturl.Hidden
	if err == nil && found {
		var vs []contenturl.Visibility
		if vs, err = r.visibility(req, []contenturl.Link{link}); err == nil {
			v = vs[0]
		}
	}
	switch {
	case err == nil && v != contenturl.Visible && v != contenturl.Gone && v != contenturl.Preview:
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no content has this code", Code: "not_found"})
	case err != nil:
		r.log.ErrorContext(req.Context(), "contenturl: resolve", "code", code, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error", Code: "internal_error"})
	case v == contenturl.Gone:
		writeJSON(w, http.StatusGone, errorBody{Error: "this content was removed", Code: "gone"})
	default:
		if v == contenturl.Preview {
			private(w)
		}
		path, _ := r.Path(link, lang)
		out := contenturl.Resolved{Link: link, Path: path}
		if v, err := ParseVersionCode(req.URL.Query().Get(contenturl.VersionParam)); err == nil {
			key := contenturl.VersionKey{ContentID: link.ContentID, Code: v}
			ids, err := r.store.Versions(req.Context(), []contenturl.VersionKey{key})
			if err != nil {
				r.log.ErrorContext(req.Context(), "contenturl: resolve version", "code", code, "version", v, "err", err)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error", Code: "internal_error"})
				return
			}
			out.VersionID = ids[key]
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (r *Router) aliases(w http.ResponseWriter, req *http.Request) {
	lang := req.URL.Query().Get("lang")
	if lang != "" && !slices.Contains(r.opts.Languages, lang) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown language", Code: "invalid_request"})
		return
	}
	var in contenturl.AliasLookup
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: httpapi.BodyError(err), Code: httpapi.CodeInvalidRequest})
		return
	}
	if len(in.Aliases) > contenturl.MaxAliases {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: fmt.Sprintf("at most %d aliases", contenturl.MaxAliases), Code: "invalid_request"})
		return
	}
	out, err := r.resolveAliases(req, in.Aliases, lang)
	switch {
	case errors.Is(err, contenturl.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: httpapi.Rule(err, httpapi.CodeInvalidRequest), Code: httpapi.CodeInvalidRequest})
	case err != nil:
		r.log.ErrorContext(req.Context(), "contenturl: resolve aliases", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error", Code: "internal_error"})
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// private marks a response only its viewer may see: no shared caching, no indexing.
func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}
