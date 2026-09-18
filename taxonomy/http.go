package taxonomy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// Handler is the admin API of one tenant's store. Hosts mount it behind their
// own admin authorization; every route is scoped to the store's tenant and
// content ids stay opaque. Errors are JSON {"error", "code"} with 400 for
// ErrInvalid, 404 for ErrNotFound, 409 for ErrConflict and a sanitized 500 for
// everything else; the cause always reaches Options.Logger.
//
//	GET    /nodes?kind=&state=&id=&slug=&language=&language_mode=
//	              &name_prefix=&q=&content_kind=&min_count=
//	              &sort=&cursor=&offset=&limit=           -> NodePage
//	POST   /nodes                [NodeInput]          -> [Node]
//	GET    /nodes/{id}                                -> NodeDetail
//	PATCH  /nodes/{id}           NodeUpdate           -> Node
//	DELETE /nodes/{id}                                -> Node (state deleted)
//	PUT    /nodes/{id}/names     [Name]               replace
//	POST   /nodes/{id}/names     [Name]               add
//	DELETE /nodes/{id}/names     [Name]               remove
//	POST   /nodes/{id}/merge     {"into_taxonomy_id"} -> MergeReport
//	POST   /edges                [Edge]  ·  DELETE /edges [Edge]
//	POST   /assignments?suppress_counts=1 [Assignment] ·  DELETE /assignments [Assignment]
//	POST   /effective            [ContentRef]         -> [{content, tags}]
//	GET    /counts?taxonomy_id=…                      -> {id: [Count]}
//	POST   /counts/rebuild                            -> {"nodes": n}
func Handler(s *Store) http.Handler {
	mux := http.NewServeMux()
	h := handler{s}
	mux.HandleFunc("GET /nodes", h.listNodes)
	mux.HandleFunc("POST /nodes", h.createNodes)
	mux.HandleFunc("GET /nodes/{id}", h.node)
	mux.HandleFunc("PATCH /nodes/{id}", h.updateNode)
	mux.HandleFunc("DELETE /nodes/{id}", h.deleteNode)
	mux.HandleFunc("PUT /nodes/{id}/names", h.names(s.SetNames))
	mux.HandleFunc("POST /nodes/{id}/names", h.names(s.AddNames))
	mux.HandleFunc("DELETE /nodes/{id}/names", h.names(s.RemoveNames))
	mux.HandleFunc("POST /nodes/{id}/merge", h.merge)
	mux.HandleFunc("POST /edges", h.edges(s.AddEdges))
	mux.HandleFunc("DELETE /edges", h.edges(s.RemoveEdges))
	mux.HandleFunc("POST /assignments", h.assignments(s.Assign))
	mux.HandleFunc("DELETE /assignments", h.assignments(s.Unassign))
	mux.HandleFunc("POST /effective", h.effective)
	mux.HandleFunc("GET /counts", h.counts)
	mux.HandleFunc("POST /counts/rebuild", h.rebuild)
	return accessLog(s.log, mux)
}

// statusWriter records the response status and the cause fail() withheld from
// the wire, for accessLog. Status defaults to 200.
type statusWriter struct {
	http.ResponseWriter
	status int
	cause  error
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// accessLog logs each admin request at DEBUG and a 5xx at ERROR, both with the
// captured cause: the only place a SQL constraint name or an internal error
// text may appear.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start)}
		if sw.cause != nil {
			attrs = append(attrs, "err", sw.cause.Error())
		}
		if sw.status >= http.StatusInternalServerError {
			log.Error("taxonomy admin request failed", attrs...)
			return
		}
		log.Debug("taxonomy admin request", attrs...)
	})
}

type handler struct{ s *Store }

const maxBody = 4 << 20

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	return nil
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Stable public error codes, the same vocabulary content uses. Clients branch
// on Code; Error is a human message and may change.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeInternal       = "internal_error"
)

// errorBody is the flat error shape of the admin API.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// fail answers with the mapped status, a stable public code and a message that
// carries nothing internal — no SQL constraint name, no driver text. The full
// cause goes to accessLog.
func fail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, CodeInternal
	switch {
	case errors.Is(err, ErrInvalid):
		status, code = http.StatusBadRequest, CodeInvalidRequest
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, CodeNotFound
	case errors.Is(err, ErrConflict):
		status, code = http.StatusConflict, CodeConflict
	}
	msg := "internal error"
	if status != http.StatusInternalServerError {
		msg = publicMessage(err)
	}
	if sw, ok := w.(*statusWriter); ok {
		sw.cause = err
	}
	write(w, status, errorBody{Error: msg, Code: code})
}

// publicMessage is the error's own text, except for a Postgres integrity
// violation, whose constraint name never leaves the process.
func publicMessage(err error) string {
	var ce constraintError
	if errors.As(err, &ce) {
		return ce.sentinel.Error()
	}
	return err.Error()
}

func (h handler) listNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	minCount, _ := strconv.Atoi(q.Get("min_count"))
	var states []State
	for _, st := range q["state"] {
		states = append(states, State(st))
	}
	var ids []TaxonomyID
	for _, id := range q["id"] {
		ids = append(ids, TaxonomyID(id))
	}
	page, err := h.s.ListNodes(r.Context(), ListOptions{
		Kind: q.Get("kind"), States: states, IDs: ids, Slug: q.Get("slug"),
		Language: q.Get("language"), LanguageMode: LanguageMode(q.Get("language_mode")),
		NamePrefix: q.Get("name_prefix"), Query: q.Get("q"),
		ContentKind: q.Get("content_kind"), MinCount: minCount,
		Sort: Sort(q.Get("sort")), Cursor: q.Get("cursor"), Offset: offset, Limit: limit,
	})
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, page)
}

func (h handler) createNodes(w http.ResponseWriter, r *http.Request) {
	var inputs []NodeInput
	if err := decode(r, &inputs); err != nil {
		fail(w, err)
		return
	}
	nodes, err := h.s.CreateNodes(r.Context(), inputs)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, nodes)
}

func (h handler) node(w http.ResponseWriter, r *http.Request) {
	d, err := h.s.Node(r.Context(), TaxonomyID(r.PathValue("id")))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, d)
}

func (h handler) updateNode(w http.ResponseWriter, r *http.Request) {
	var update NodeUpdate
	if err := decode(r, &update); err != nil {
		fail(w, err)
		return
	}
	n, err := h.s.UpdateNode(r.Context(), TaxonomyID(r.PathValue("id")), update)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, n)
}

func (h handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	state := StateDeleted
	n, err := h.s.UpdateNode(r.Context(), TaxonomyID(r.PathValue("id")), NodeUpdate{State: &state})
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, n)
}

func (h handler) names(op func(context.Context, TaxonomyID, []Name) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var names []Name
		if err := decode(r, &names); err != nil {
			fail(w, err)
			return
		}
		if err := op(r.Context(), TaxonomyID(r.PathValue("id")), names); err != nil {
			fail(w, err)
			return
		}
		d, err := h.s.Node(r.Context(), TaxonomyID(r.PathValue("id")))
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, d)
	}
}

func (h handler) merge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Into TaxonomyID `json:"into_taxonomy_id"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, err)
		return
	}
	report, err := h.s.Merge(r.Context(), TaxonomyID(r.PathValue("id")), body.Into)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, report)
}

func (h handler) edges(op func(context.Context, []Edge) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var edges []Edge
		if err := decode(r, &edges); err != nil {
			fail(w, err)
			return
		}
		if err := op(r.Context(), edges); err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, map[string]int{"edges": len(edges)})
	}
}

func (h handler) assignments(op func(context.Context, []Assignment, AssignOptions) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var assignments []Assignment
		if err := decode(r, &assignments); err != nil {
			fail(w, err)
			return
		}
		for i := range assignments {
			if assignments[i].TenantID == "" {
				assignments[i].TenantID = h.s.tenant
			}
		}
		suppress, _ := strconv.ParseBool(r.URL.Query().Get("suppress_counts"))
		if err := op(r.Context(), assignments, AssignOptions{SuppressCounts: suppress}); err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, map[string]int{"assignments": len(assignments)})
	}
}

// EffectiveTagsOf is one reference with its effective tags.
type EffectiveTagsOf struct {
	Content contentref.ContentRef `json:"content"`
	Tags    []EffectiveTag        `json:"tags"`
}

func (h handler) effective(w http.ResponseWriter, r *http.Request) {
	var refs []contentref.ContentRef
	if err := decode(r, &refs); err != nil {
		fail(w, err)
		return
	}
	for i := range refs {
		if refs[i].TenantID == "" {
			refs[i].TenantID = h.s.tenant
		}
	}
	tags, err := h.s.EffectiveTags(r.Context(), refs)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]EffectiveTagsOf, 0, len(refs))
	for _, ref := range refs {
		t := tags[ref.Key()]
		if t == nil {
			t = []EffectiveTag{}
		}
		out = append(out, EffectiveTagsOf{Content: ref, Tags: t})
	}
	write(w, http.StatusOK, out)
}

func (h handler) counts(w http.ResponseWriter, r *http.Request) {
	var ids []TaxonomyID
	for _, id := range r.URL.Query()["taxonomy_id"] {
		ids = append(ids, TaxonomyID(id))
	}
	counts, err := h.s.Counts(r.Context(), ids)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, counts)
}

func (h handler) rebuild(w http.ResponseWriter, r *http.Request) {
	n, err := h.s.RebuildCounts(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]int{"nodes": n})
}
