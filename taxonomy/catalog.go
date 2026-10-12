package taxonomy

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// CatalogOptions configures the public catalog API.
type CatalogOptions struct {
	// Scope derives a request's scope, such as the host's site flavor for its
	// Host header; nil totals the whole catalog. An error wrapping ErrInvalid
	// answers 400, any other 500.
	Scope func(*http.Request) (CatalogScope, error)
}

// CatalogHandler is the public catalog API (catalogRoutes), open to anyone:
// GET /totals sizes the listing by kind (Totals) and may be cached by any
// cache for ten minutes. contentkit.Runtime.Handler serves it at /catalog. It
// needs Options.Listing.
func (s *Store) CatalogHandler(opts CatalogOptions) (http.Handler, error) {
	if s.listing == nil {
		return nil, fmt.Errorf("%w: CatalogHandler needs Options.Listing", ErrInvalid)
	}
	mux := http.NewServeMux()
	httpapi.Mount(mux, nil, catalogHandler{s, opts}, catalogRoutes)
	return accessLog(s.log, "public catalog", mux), nil
}

func init() { httpapi.Register(httpapi.PublicCatalog, catalogRoutes) }

var catalogRoutes = []httpapi.Route[catalogHandler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/totals", Resource: "catalog", Auth: httpapi.Open,
		Doc:       "Each kind's works: how many the anonymous listing shows that anyone opens, and how many each tier's holders see and open. Cacheable for ten minutes.",
		Query:     []httpapi.Param{httpapi.List("kinds", "content kinds to total; every listed kind by default")},
		Responses: []httpapi.Reply{httpapi.OK(Totals{})}, Errors: []string{httpapi.CodeUnavailable}},
		Serve: httpapi.H(catalogHandler.totals)},
}

type catalogHandler struct {
	s    *Store
	opts CatalogOptions
}

func (h catalogHandler) totals(w http.ResponseWriter, r *http.Request) {
	var q TotalsQuery
	for _, k := range strings.Split(strings.Join(r.URL.Query()["kinds"], ","), ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if !slices.Contains(h.s.listing.kinds, k) {
			fail(w, fmt.Errorf("%w: kind %q is not listed", ErrInvalid, k))
			return
		}
		q.Kinds = append(q.Kinds, k)
	}
	if h.opts.Scope != nil {
		var err error
		if q.Scope, err = h.opts.Scope(r); err != nil {
			fail(w, err)
			return
		}
	}
	t, err := h.s.Totals(r.Context(), q)
	switch {
	case errors.Is(err, ErrInvalid):
		fail(w, fmt.Errorf("catalog scope: %v", err)) // the host's scope is at fault, not the request: a 500
	case err != nil:
		if sw, ok := w.(*statusWriter); ok {
			sw.cause = err
		}
		write(w, http.StatusServiceUnavailable, errorBody{Error: "catalog totals are unavailable", Code: httpapi.CodeUnavailable})
	default:
		w.Header().Set("Cache-Control", "public, max-age=600")
		write(w, http.StatusOK, t)
	}
}
