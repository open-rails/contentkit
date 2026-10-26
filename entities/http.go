package entities

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

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Handler serves the entities API (routes): listings and entity pages, each
// with the viewer's standing with entitlement-backed entities; creator writes of
// user-owned entities (As); and staff writes for holders of
// access.EntitiesWrite in a's root scope. Errors are JSON {"error", "code"}:
// 400 for ErrInvalid, 403 for ErrForbidden, 404 for ErrNotFound, 409 for
// ErrConflict and ErrLimit (limit_reached), 503 when auth or billing is
// down and a sanitized 500 otherwise; the cause always reaches
// Options.Logger.
func (s *Store) Handler(a auth.Authenticator) (http.Handler, error) {
	if err := gate.Check(a, access.EntitiesWrite); err != nil {
		return nil, fmt.Errorf("entities: Handler: %w", err)
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: a, Log: s.log, Then: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(access.WithMemo(r.Context())))
		})
	}}
	httpapi.Mount(mux, g.Guard, handler{s: s, auth: a}, routes)
	return accessLog(s.log, mux), nil
}

func init() { httpapi.Register(httpapi.Entities, routes) }

var (
	language = httpapi.Text("language", "the language names, text and slugs resolve in, counts are scoped to and paths are prefixed with")
	notFound = []string{httpapi.CodeNotFound}
	writes   = []string{httpapi.CodeConflict, httpapi.CodeNotFound}
	creates  = []string{httpapi.CodeConflict, httpapi.CodeNotFound, httpapi.CodeLimitReached}
)

const editors = " Staff holding root:entities:write write any entity; a signed-in person creates entities of user-owned kinds, as their owner and within the kind's max_per_owner, and the owner and holders of {kind}:settings:manage in its AuthKit scope edit names, text, attributes, cover, avatar and state."

var routes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/entities", Resource: "entities", Auth: httpapi.Public,
		Doc: "Lists active entities with name, code, path, cover, avatar, count and the viewer's entitlement membership, filtered, ordered and cursor-paged.",
		Query: []httpapi.Param{httpapi.Text("kind", "only entities of this kind"), httpapi.Text("parent", "only children of this entity"),
			httpapi.Text("scope", "only entities scoped by this entity"), httpapi.Text("slug", "only the entity with this slug"),
			httpapi.Text("letter", "only sort names starting with this letter (a-z), or with anything else (#)"), language,
			httpapi.Text("content_kind", "count contents of this kind; every kind by default"),
			httpapi.Text("sort", "name (the default), count or newest"), httpapi.Text("cursor", "the previous page's next_cursor"),
			httpapi.Int("page", "a numbered page from 1 instead of cursor, with the total; page × limit is at most 10,000"),
			httpapi.Int("limit", "page size: 50 by default, at most 200")},
		Responses: []httpapi.Reply{httpapi.OK(EntityPage{})}}, Serve: httpapi.H(handler.list)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/entities/{id}", Resource: "entities", Auth: httpapi.Public,
		Doc:   "An active entity with its names, text, attributes, parent, scope, children, cover, avatar, edges, counts and membership standing; a merged entity answers with its survivor.",
		Query: []httpapi.Param{language}, Responses: []httpapi.Reply{httpapi.OK(EntityDetail{})}, Errors: notFound}, Serve: httpapi.H(handler.detail)},
	staff(httpapi.GET, "/manage/entities", "Lists active (or deleted) entities for administration, including internal classifications that are never exposed by public reads.", nil, httpapi.OK(EntityPage{}), httpapi.H(handler.manageList), nil,
		httpapi.Text("state", "active (the default) or deleted, the restorable ones"), httpapi.Text("kind", "only entities of this kind"), httpapi.Text("parent", "only children of this entity"), httpapi.Text("scope", "only entities scoped by this entity"),
		httpapi.Text("slug", "only the entity with this slug"), httpapi.Text("q", "match a canonical name, alias or slug"), httpapi.Text("letter", "only sort names starting with this letter"), language,
		httpapi.Text("content_kind", "count contents of this kind"), httpapi.Text("sort", "name, count or newest"), httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "page size: at most 200")),
	staff(httpapi.GET, "/manage/entities/{id}", "An entity's administration view, including internal classifications and their relationships.", nil, httpapi.OK(EntityDetail{}), httpapi.H(handler.manageDetail), notFound, language),

	editor(httpapi.POST, "/entities", "Creates entities with their names and text; an auth_scope kind's gets its AuthKit scope."+editors, []EntityInput{}, httpapi.Created([]Entity{}),
		httpapi.H(handler.create), creates),
	editor(httpapi.PATCH, "/entities/{id}", "Updates a entity's given fields; state restores or deletes it."+editors, EntityUpdate{}, httpapi.OK(Entity{}), httpapi.H(handler.update), creates),
	editor(httpapi.DELETE, "/entities/{id}", "Marks an entity deleted and retires its AuthKit scope; a user-owned entity's content is deleted with it, and restoring the entity (PATCH state active) brings that content back within retention. Declared public media remains public until permanent purge."+editors, nil, httpapi.OK(Entity{}), httpapi.H(handler.remove), notFound),
	staff(httpapi.PUT, "/entities/{id}/names", "Replaces a entity's names and aliases.", []Name{}, httpapi.OK([]Name{}),
		func(h handler) http.HandlerFunc { return h.names(h.s.SetNames) }, writes),
	staff(httpapi.POST, "/entities/{id}/names", "Adds names and aliases to a entity; a new canonical name demotes the old one to an alias.", []Name{}, httpapi.OK([]Name{}),
		func(h handler) http.HandlerFunc { return h.names(h.s.AddNames) }, writes),
	staff(httpapi.DELETE, "/entities/{id}/names", "Removes names and aliases from a entity.", []Name{}, httpapi.OK([]Name{}),
		func(h handler) http.HandlerFunc { return h.names(h.s.RemoveNames) }, writes),
	staff(httpapi.POST, "/entities/{id}/edges", "Adds edges from a entity.", []EdgeInput{}, httpapi.OK([]Edge{}),
		func(h handler) http.HandlerFunc { return h.edges(h.s.AddEdges) }, writes),
	staff(httpapi.DELETE, "/entities/{id}/edges", "Removes edges from a entity.", []EdgeInput{}, httpapi.OK([]Edge{}),
		func(h handler) http.HandlerFunc { return h.edges(h.s.RemoveEdges) }, nil),
	staff(httpapi.POST, "/entities/{id}/merge", "Merges a entity into another of its kind: memberships, names, edges and children move, and its code redirects.",
		MergeInput{}, httpapi.OK(MergeReport{}), httpapi.H(handler.merge), writes),
	staff(httpapi.GET, "/entities/{id}/members", "A entity's memberships in member order, paged.", nil, httpapi.OK(MemberPage{}), httpapi.H(handler.members), notFound,
		httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "page size: 50 by default, at most 500")),
	staff(httpapi.POST, "/entities/{id}/members", "Adds contents or versions to a entity, or updates their position and state.", []Membership{}, httpapi.OK(Written{}),
		httpapi.H(handler.addMembers), writes),
	staff(httpapi.DELETE, "/entities/{id}/members", "Removes contents or versions from a entity.", []Membership{}, httpapi.OK(Written{}), httpapi.H(handler.removeMembers), nil),
	staff(httpapi.PUT, "/entities/{id}/members/order", "Orders an ordered entity: the listed members first, in list order.", Order{}, httpapi.OK(Written{}),
		httpapi.H(handler.reorder), notFound),
	staff(httpapi.GET, "/contents/{id}/entities", "A content's memberships, its versions' included, with the entities they name.", nil, httpapi.OK(ContentMemberships{}),
		httpapi.H(handler.contentEntities), nil, language),
	staff(httpapi.PUT, "/contents/{id}/entities", "Replaces a content's memberships, its versions' included, in the entity kinds named (every kind its kind joins by default).",
		ContentEntities{}, httpapi.OK(ContentMemberships{}), httpapi.H(handler.setContentEntities), writes, language),
	staff(httpapi.PUT, "/contents/{id}/entities/shared", "Replaces only shared content memberships in the named entity kinds, preserving every version's memberships. Entity kinds must allow the content membership scope; omitted kinds select all such kinds. New names attach to the shared content.",
		ContentEntities{}, httpapi.OK(ContentMemberships{}), httpapi.H(handler.setSharedContentEntities), writes, language),
	staff(httpapi.PUT, "/contents/{id}/versions/{vid}/entities", "Replaces only this version's memberships in the named entity kinds, preserving shared and sibling-version memberships. Entity kinds must allow the version membership scope; omitted kinds select all such kinds. New names attach to this version. The version must belong to the content and not be deleted.",
		ContentEntities{}, httpapi.OK(ContentMemberships{}), httpapi.H(handler.setVersionContentEntities), writes, language),
}

func editor(method, path, doc string, request any, reply httpapi.Reply, serve func(handler) http.HandlerFunc, errs []string) httpapi.Route[handler] {
	return httpapi.Route[handler]{Spec: httpapi.Spec{Method: method, Path: path, Resource: "entities", Doc: doc, Auth: httpapi.Editor,
		Perm: string(access.EntitiesWrite), Request: request, Responses: []httpapi.Reply{reply}, Errors: errs}, Serve: serve}
}

func staff(method, path, doc string, request any, reply httpapi.Reply, serve func(handler) http.HandlerFunc, errs []string, query ...httpapi.Param) httpapi.Route[handler] {
	return httpapi.Route[handler]{Spec: httpapi.Spec{Method: method, Path: path, Resource: "entities", Doc: doc, Auth: httpapi.Staff,
		Perm: string(access.EntitiesWrite), Query: query, Request: request, Responses: []httpapi.Reply{reply}, Errors: errs}, Serve: serve}
}

// Written counts the items a write named.
type Written struct {
	Count int `json:"count"`
}

// ContentMemberships is a content's memberships with the entities they name.
type ContentMemberships struct {
	ContentID   string       `json:"content_id"`
	Memberships []Membership `json:"memberships"`
	Entities    []EntityCard `json:"entities"`
}

type handler struct {
	s    *Store
	auth auth.Authenticator
}

// as is the store acting as the request's actor.
func (h handler) as(r *http.Request) *Store { return h.s.As(h.auth, gate.Actor(r.Context())) }

// statusWriter records the status and the cause fail() withheld from the
// wire, for accessLog.
type statusWriter struct {
	http.ResponseWriter
	status int
	cause  error
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// accessLog logs each request at DEBUG and a 5xx at ERROR, both with the
// captured cause: the only place a constraint name or internal error appears.
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
			log.Error("groups request failed", attrs...)
			return
		}
		log.Debug("groups request", attrs...)
	})
}

const maxBody = 4 << 20

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, httpapi.BodyError(err))
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: the body holds more than one JSON value", ErrInvalid)
	}
	return nil
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// fail answers the mapped status and stable code with a message that carries
// nothing internal; the full cause goes to accessLog.
func fail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, httpapi.CodeInternal
	switch {
	case errors.Is(err, ErrInvalid):
		status, code = http.StatusBadRequest, httpapi.CodeInvalidRequest
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, httpapi.CodeNotFound
	case errors.Is(err, ErrConflict):
		status, code = http.StatusConflict, httpapi.CodeConflict
	case errors.Is(err, ErrLimit):
		status, code = http.StatusConflict, httpapi.CodeLimitReached
	case errors.Is(err, ErrForbidden):
		status, code = http.StatusForbidden, httpapi.CodeForbidden
	case errors.Is(err, auth.ErrUnauthenticated):
		status, code = http.StatusUnauthorized, httpapi.CodeUnauthorized
	case errors.Is(err, auth.ErrUnavailable), errors.Is(err, access.ErrUnavailable):
		status, code = http.StatusServiceUnavailable, httpapi.CodeUnavailable
	}
	msg := httpapi.Message(code)
	var binding *policyBindingError
	if errors.As(err, &binding) {
		msg = binding.Error()
	}
	var ce constraintError
	if code == httpapi.CodeInvalidRequest && !errors.As(err, &ce) {
		msg = httpapi.Rule(err, code)
	}
	if sw, ok := w.(*statusWriter); ok {
		sw.cause = err
	}
	write(w, status, errorBody{Error: msg, Code: code})
}

func reply(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	write(w, status, v)
}

func pathID(r *http.Request) EntityID { return EntityID(r.PathValue("id")) }

func (h handler) list(w http.ResponseWriter, r *http.Request) {
	h.listEntities(w, r, false)
}

func (h handler) manageList(w http.ResponseWriter, r *http.Request) {
	h.listEntities(w, r, true)
}

func (h handler) listEntities(w http.ResponseWriter, r *http.Request, management bool) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page := 0
	if v := q.Get("page"); v != "" {
		var err error
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			reply(w, http.StatusOK, EntityPage{}, fmt.Errorf("%w: page", ErrInvalid))
			return
		}
	}
	opts := ListOptions{Kind: q.Get("kind"), Parent: EntityID(q.Get("parent")), Scope: EntityID(q.Get("scope")),
		Slug: q.Get("slug"), Letter: q.Get("letter"), Language: q.Get("language"), ContentKind: q.Get("content_kind"),
		Sort: Sort(q.Get("sort")), Cursor: q.Get("cursor"), Page: page, Limit: limit, IncludeInternal: management}
	if management {
		opts.Query = q.Get("q")
		switch q.Get("state") {
		case "", string(StateActive):
		case string(StateDeleted):
			opts.Deleted = true
		default:
			reply(w, http.StatusOK, EntityPage{}, fmt.Errorf("%w: state is active or deleted", ErrInvalid))
			return
		}
	}
	out, err := h.as(r).List(r.Context(), opts)
	reply(w, http.StatusOK, out, err)
}

func (h handler) mine(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := h.as(r).Mine(r.Context(), ListOptions{Kind: q.Get("kind"), Language: q.Get("language"), Sort: Sort(q.Get("sort")),
		Cursor: q.Get("cursor"), Limit: limit})
	reply(w, http.StatusOK, page, err)
}

func (h handler) detail(w http.ResponseWriter, r *http.Request) {
	d, err := h.as(r).Detail(r.Context(), pathID(r), r.URL.Query().Get("language"))
	reply(w, http.StatusOK, d, err)
}

func (h handler) manageDetail(w http.ResponseWriter, r *http.Request) {
	d, err := h.as(r).ManagementDetail(r.Context(), pathID(r), r.URL.Query().Get("language"))
	reply(w, http.StatusOK, d, err)
}

func (h handler) create(w http.ResponseWriter, r *http.Request) {
	var in []EntityInput
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	out, err := h.as(r).CreateEntities(r.Context(), in)
	reply(w, http.StatusCreated, out, err)
}

func (h handler) update(w http.ResponseWriter, r *http.Request) {
	var u EntityUpdate
	if err := decode(r, &u); err != nil {
		fail(w, err)
		return
	}
	u.ID = pathID(r)
	h.one(w, func() ([]Entity, error) { return h.as(r).UpdateEntities(r.Context(), []EntityUpdate{u}) })
}

func (h handler) remove(w http.ResponseWriter, r *http.Request) {
	h.one(w, func() ([]Entity, error) { return h.as(r).DeleteEntities(r.Context(), []EntityID{pathID(r)}) })
}

func (h handler) one(w http.ResponseWriter, fn func() ([]Entity, error)) {
	out, err := fn()
	if err == nil && len(out) != 1 {
		err = ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, out[0])
}

func (h handler) names(op func(context.Context, []EntityNames) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var names []Name
		if err := decode(r, &names); err != nil {
			fail(w, err)
			return
		}
		id := pathID(r)
		if err := op(r.Context(), []EntityNames{{ID: id, Names: names}}); err != nil {
			fail(w, err)
			return
		}
		var out []Name
		err := h.s.read(r.Context(), func(q Querier) error {
			all, err := h.s.names(r.Context(), q, []string{string(id)})
			out = all[id]
			return err
		})
		if out == nil {
			out = []Name{}
		}
		reply(w, http.StatusOK, out, err)
	}
}

func (h handler) edges(op func(context.Context, []Edge) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in []EdgeInput
		if err := decode(r, &in); err != nil {
			fail(w, err)
			return
		}
		id := pathID(r)
		if err := validateID(id); err != nil {
			fail(w, err)
			return
		}
		edges := make([]Edge, len(in))
		for i, e := range in {
			edges[i] = Edge{From: id, Relation: e.Relation, To: e.To}
		}
		if err := op(r.Context(), edges); err != nil {
			fail(w, err)
			return
		}
		var out []Edge
		err := h.s.read(r.Context(), func(q Querier) (err error) {
			out, err = h.s.edges(r.Context(), q, []string{string(id)})
			return err
		})
		reply(w, http.StatusOK, out, err)
	}
}

func (h handler) merge(w http.ResponseWriter, r *http.Request) {
	var in MergeInput
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	reports, err := h.s.Merge(r.Context(), []Merge{{From: pathID(r), Into: in.Into}})
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, reports[0])
}

func (h handler) members(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := h.s.Members(r.Context(), pathID(r), MemberOptions{Cursor: r.URL.Query().Get("cursor"), Limit: limit})
	reply(w, http.StatusOK, page, err)
}

// groupMembers decodes memberships of the path's entity.
func entityMembers(r *http.Request) ([]Membership, error) {
	var ms []Membership
	if err := decode(r, &ms); err != nil {
		return nil, err
	}
	id := pathID(r)
	for i := range ms {
		if ms[i].EntityID != "" && ms[i].EntityID != id {
			return nil, fmt.Errorf("%w: a membership names group %s", ErrInvalid, ms[i].EntityID)
		}
		ms[i].EntityID = id
	}
	return ms, nil
}

func (h handler) addMembers(w http.ResponseWriter, r *http.Request) {
	ms, err := entityMembers(r)
	if err == nil {
		err = h.s.AddMemberships(r.Context(), ms)
	}
	reply(w, http.StatusOK, Written{Count: len(ms)}, err)
}

func (h handler) removeMembers(w http.ResponseWriter, r *http.Request) {
	ms, err := entityMembers(r)
	if err == nil {
		err = h.s.RemoveMemberships(r.Context(), ms)
	}
	reply(w, http.StatusOK, Written{Count: len(ms)}, err)
}

func (h handler) reorder(w http.ResponseWriter, r *http.Request) {
	var o Order
	if err := decode(r, &o); err != nil {
		fail(w, err)
		return
	}
	o.EntityID = pathID(r)
	reply(w, http.StatusOK, Written{Count: len(o.Members)}, h.s.Reorder(r.Context(), []Order{o}))
}

func (h handler) contentEntities(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.contentMemberships(r.Context(), r.PathValue("id"), r.URL.Query().Get("language"))
	reply(w, http.StatusOK, out, err)
}

func (h handler) setContentEntities(w http.ResponseWriter, r *http.Request) {
	h.setContentEntitiesScoped(w, r, nil)
}

func (h handler) setSharedContentEntities(w http.ResponseWriter, r *http.Request) {
	shared := ""
	h.setContentEntitiesScoped(w, r, &shared)
}

func (h handler) setVersionContentEntities(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("vid")
	h.setContentEntitiesScoped(w, r, &version)
}

func (h handler) setContentEntitiesScoped(w http.ResponseWriter, r *http.Request, versionScope *string) {
	var set ContentEntities
	if err := decode(r, &set); err != nil {
		fail(w, err)
		return
	}
	set.ContentID = r.PathValue("id")
	set.VersionScope = versionScope
	if err := h.s.SetContentEntities(r.Context(), []ContentEntities{set}); err != nil {
		fail(w, err)
		return
	}
	h.contentEntities(w, r)
}

// contentMemberships is one content's memberships with their entities' cards.
func (s *Store) contentMemberships(ctx context.Context, contentID, language string) (ContentMemberships, error) {
	out := ContentMemberships{ContentID: contentID, Memberships: []Membership{}, Entities: []EntityCard{}}
	all, err := s.MembershipsOf(ctx, []string{contentID})
	if err != nil {
		return out, err
	}
	if ms := all[contentID]; ms != nil {
		out.Memberships = ms
	}
	ids := []string{}
	for _, m := range out.Memberships {
		ids = append(ids, string(m.EntityID))
	}
	if len(ids) == 0 {
		return out, s.read(ctx, func(q Querier) error {
			var exists bool
			if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE tenant_id = $1 AND id = $2)`, s.table("content_items")), s.tenant, contentID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: content %s", ErrNotFound, contentID)
			}
			return nil
		})
	}
	c, err := s.compile(ListOptions{Language: language, Limit: 200, IncludeInternal: true}, ids)
	if err != nil {
		return out, err
	}
	c.limit, c.args["limit"] = len(ids), len(ids)+1
	err = s.read(ctx, func(q Querier) error {
		page, err := s.cards(ctx, q, c)
		out.Entities = page.Entities
		return err
	})
	return out, err
}
