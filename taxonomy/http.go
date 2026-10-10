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
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Handler is the admin API of one tenant's store (taxonomyRoutes). Every
// route is scoped to the store's tenant and content ids stay opaque.
// contentkit.Runtime.Handler serves it at /taxonomy behind
// content.Perms.Taxonomy; a host mounting it alone puts it behind its own
// admin authorization. Errors are JSON {"error", "code"} with 400 for
// ErrInvalid, 404 for ErrNotFound, 409 for ErrConflict and a sanitized 500 for
// everything else; the cause always reaches Options.Logger.
func Handler(s *Store) http.Handler {
	mux := http.NewServeMux()
	httpapi.Mount(mux, handler{s}, taxonomyRoutes)
	return accessLog(s.log, mux)
}

func init() { httpapi.Register(httpapi.Taxonomy, taxonomyRoutes) }

var taxonomyRoutes = []httpapi.Route[handler]{
	adminRoute(httpapi.GET, "/nodes", "Lists nodes: filtered, ordered and paged.", nil, okReply(NodePage{}), httpapi.H(handler.listNodes), nil,
		httpapi.Text("kind", "only nodes of this kind"), httpapi.Repeated("state", "only nodes in these states; active by default"),
		httpapi.Repeated("id", "only these taxonomy ids"), httpapi.Text("slug", "only the node with this slug"),
		httpapi.Text("language", "the language names resolve in"),
		httpapi.Text("language_mode", "what a node without a name in language gets: fallback (the default), strict or required"),
		httpapi.Text("name_prefix", "names starting with this"), httpapi.Text("q", "names containing this"),
		httpapi.Text("content_kind", "count contents of this kind"), httpapi.Int("min_count", "only nodes with at least this many contents"),
		httpapi.Text("sort", "name, count, created, oldest or updated; taxonomy id by default"),
		httpapi.Text("cursor", "the previous page's next_cursor (default order only)"),
		httpapi.Int("offset", "nodes to skip"), httpapi.Int("limit", "page size")),
	adminRoute(httpapi.POST, "/nodes", "Creates nodes with their names.", []NodeInput{}, []httpapi.Reply{httpapi.Created([]Node{})},
		httpapi.H(handler.createNodes), []string{CodeConflict}),
	adminRoute(httpapi.GET, "/nodes/{id}", "A node with its names and edges.", nil, okReply(NodeDetail{}), httpapi.H(handler.node), []string{CodeNotFound}),
	adminRoute(httpapi.PATCH, "/nodes/{id}", "Updates a node's given fields.", NodeUpdate{}, okReply(Node{}), httpapi.H(handler.updateNode), []string{CodeConflict, CodeNotFound}),
	adminRoute(httpapi.DELETE, "/nodes/{id}", "Marks a node deleted.", nil, okReply(Node{}), httpapi.H(handler.deleteNode), []string{CodeNotFound}),
	adminRoute(httpapi.PUT, "/nodes/{id}/names", "Replaces a node's names.", []Name{}, okReply(NodeDetail{}),
		func(h handler) http.HandlerFunc { return h.names(h.s.SetNames) }, []string{CodeConflict, CodeNotFound}),
	adminRoute(httpapi.POST, "/nodes/{id}/names", "Adds names to a node.", []Name{}, okReply(NodeDetail{}),
		func(h handler) http.HandlerFunc { return h.names(h.s.AddNames) }, []string{CodeConflict, CodeNotFound}),
	adminRoute(httpapi.DELETE, "/nodes/{id}/names", "Removes names from a node.", []Name{}, okReply(NodeDetail{}),
		func(h handler) http.HandlerFunc { return h.names(h.s.RemoveNames) }, []string{CodeNotFound}),
	adminRoute(httpapi.POST, "/nodes/{id}/merge", "Merges a node into another: its assignments, names and edges move, and its URL redirects.",
		MergeInput{}, okReply(MergeReport{}), httpapi.H(handler.merge), []string{CodeConflict, CodeNotFound}),
	adminRoute(httpapi.POST, "/edges", "Adds edges.", []Edge{}, okReply(EdgesWritten{}),
		func(h handler) http.HandlerFunc { return h.edges(h.s.AddEdges) }, []string{CodeConflict, CodeNotFound}),
	adminRoute(httpapi.DELETE, "/edges", "Removes edges.", []Edge{}, okReply(EdgesWritten{}),
		func(h handler) http.HandlerFunc { return h.edges(h.s.RemoveEdges) }, nil),
	adminRoute(httpapi.POST, "/assignments", "Assigns nodes to contents.", []Assignment{}, okReply(AssignmentsWritten{}),
		func(h handler) http.HandlerFunc { return h.assignments(h.s.Assign) }, []string{CodeConflict, CodeNotFound}, suppressCounts),
	adminRoute(httpapi.DELETE, "/assignments", "Removes assignments.", []Assignment{}, okReply(AssignmentsWritten{}),
		func(h handler) http.HandlerFunc { return h.assignments(h.s.Unassign) }, nil, suppressCounts),
	adminRoute(httpapi.POST, "/effective", "The effective tags (work and version) of each content.", []contentref.ContentRef{}, okReply([]EffectiveTagsOf{}),
		httpapi.H(handler.effective), nil),
	adminRoute(httpapi.GET, "/counts", "Content counts of nodes, by taxonomy id.", nil, okReply(map[TaxonomyID][]Count{}), httpapi.H(handler.counts), nil,
		httpapi.Repeated("taxonomy_id", "the nodes to count")),
	adminRoute(httpapi.POST, "/counts/rebuild", "Recomputes every count, after bulk loads written with suppress_counts.", nil, okReply(CountsRebuilt{}),
		httpapi.H(handler.rebuild), nil),
}

var suppressCounts = httpapi.Bool("suppress_counts", "skip count maintenance; rebuild the counts afterwards")

func okReply(body any) []httpapi.Reply { return []httpapi.Reply{httpapi.OK(body)} }

func adminRoute(method, path, doc string, request any, replies []httpapi.Reply, serve func(handler) http.HandlerFunc, errs []string, query ...httpapi.Param) httpapi.Route[handler] {
	return httpapi.Route[handler]{Spec: httpapi.Spec{Method: method, Path: path, Resource: "taxonomy", Doc: doc,
		Auth: httpapi.Staff, Perm: "Taxonomy", Query: query, Request: request, Responses: replies, Errors: errs}, Serve: serve}
}

// MergeInput names the node a merge folds into.
type MergeInput struct {
	Into TaxonomyID `json:"into_taxonomy_id"`
}

// EdgesWritten counts the edges of an edge write.
type EdgesWritten struct {
	Edges int `json:"edges"`
}

// AssignmentsWritten counts the assignments of an assignment write.
type AssignmentsWritten struct {
	Assignments int `json:"assignments"`
}

// CountsRebuilt counts the nodes a rebuild recounted.
type CountsRebuilt struct {
	Nodes int `json:"nodes"`
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
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%w: body: multiple JSON values", ErrInvalid)
		}
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
	var body MergeInput
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
		write(w, http.StatusOK, EdgesWritten{Edges: len(edges)})
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
		write(w, http.StatusOK, AssignmentsWritten{Assignments: len(assignments)})
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
	write(w, http.StatusOK, CountsRebuilt{Nodes: n})
}
