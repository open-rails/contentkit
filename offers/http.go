package offers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Handler serves the offers routes: creator and staff offer edits, free
// group joins, the viewer's library and the staff offer list. a says who
// each request is; each request runs in access.WithMemo.
func (s *Service) Handler(a auth.Authenticator) (http.Handler, error) {
	if err := gate.Check(a, access.OffersWrite); err != nil {
		return nil, fmt.Errorf("offers: Handler: %w", err)
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: a, Log: s.log, Then: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(access.WithMemo(r.Context())))
		})
	}}
	httpapi.Mount(mux, g.Guard, handler{s: s, auth: a}, routes)
	return mux, nil
}

func init() { httpapi.Register(httpapi.Offers, routes) }

type handler struct {
	s    *Service
	auth auth.Authenticator
}

// codeNotJoinable answers a join of a group without a free offer.
const codeNotJoinable = "not_joinable"

const (
	contentEditors = " For creators of the content, editors of its owner group (its owner, holders of {kind}:offers:write or {kind}:settings:manage in its AuthKit scope) and staff holding root:offers:write."
	groupEditors   = " For the group's owner, holders of {kind}:offers:write or {kind}:settings:manage in its AuthKit scope and staff holding root:offers:write."
)

var routes = []httpapi.Route[handler]{
	edit(httpapi.PUT, "/contents/{id}/offer", "Puts an offer on a content item: its content key opens every version."+contentEditors, Terms{}, putOffer(contentTarget)),
	edit(httpapi.DELETE, "/contents/{id}/offer", "Withdraws a content item's offer; buyers keep it."+contentEditors, nil, withdrawOffer(contentTarget)),
	edit(httpapi.PUT, "/contents/{id}/versions/{vid}/offer", "Puts an offer on one version: its version key opens it."+contentEditors, Terms{}, putOffer(versionTarget)),
	edit(httpapi.DELETE, "/contents/{id}/versions/{vid}/offer", "Withdraws a version's offer; buyers keep it."+contentEditors, nil, withdrawOffer(versionTarget)),
	edit(httpapi.PUT, "/groups/{id}/offer", "Puts an offer on a sellable group: its key opens its members and its members-only content. renew_hours makes it a subscription; amount 0 makes it free to join. Terms without renew_hours stop existing subscriptions renewing."+groupEditors,
		Terms{}, putOffer(groupTarget)),
	edit(httpapi.DELETE, "/groups/{id}/offer", "Withdraws a group's offer; members keep theirs, subscribers until their paid period ends: no subscription renews."+groupEditors, nil, withdrawOffer(groupTarget)),
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/groups/{id}/join", Resource: "offers", Doc: "Joins a group free to join: grants the viewer its key.",
		Auth: httpapi.User, Responses: []httpapi.Reply{httpapi.OK(GroupJoin{})}, Errors: []string{httpapi.CodeNotFound, codeNotJoinable}},
		Serve: httpapi.H(handler.join)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/groups/{id}/join", Resource: "offers", Doc: "Leaves a group joined free; a bought membership stays.",
		Auth: httpapi.User, Responses: []httpapi.Reply{httpapi.OK(GroupJoin{})}, Errors: []string{httpapi.CodeNotFound, httpapi.CodeConflict}},
		Serve: httpapi.H(handler.leave)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/library", Resource: "offers", Doc: "The content the viewer bought, newest first, deleted content included.",
		Auth: httpapi.User, Query: []httpapi.Param{httpapi.Text("language", "the language of the titles"),
			httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "page size: 20 by default, at most 100")},
		Responses: []httpapi.Reply{httpapi.OK(LibraryPage{})}}, Serve: httpapi.H(handler.library)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/offers", Resource: "offers", Doc: "Offers, newest first, with their OpenRails sync state.",
		Auth: httpapi.Staff, Perm: string(access.OffersWrite), Query: []httpapi.Param{httpapi.Text("state", "active or withdrawn"),
			httpapi.Bool("unsynced", "only offers OpenRails does not match yet"), httpapi.Text("cursor", "the previous page's next_cursor"),
			httpapi.Int("limit", "page size: 50 by default, at most 100")},
		Responses: []httpapi.Reply{httpapi.OK(OfferPage{})}}, Serve: httpapi.H(handler.list)},
}

func edit(method, path, doc string, body any, serve func(handler, http.ResponseWriter, *http.Request)) httpapi.Route[handler] {
	return httpapi.Route[handler]{Spec: httpapi.Spec{Method: method, Path: path, Resource: "offers", Doc: doc, Auth: httpapi.Editor,
		Perm: string(access.OffersWrite), Request: body, Responses: []httpapi.Reply{httpapi.OK(Offer{})}, Errors: []string{httpapi.CodeNotFound, httpapi.CodeConflict}},
		Serve: httpapi.H(serve)}
}

func contentTarget(r *http.Request) Target { return Target{Kind: ContentTarget, ID: r.PathValue("id")} }
func versionTarget(r *http.Request) Target {
	return Target{Kind: VersionTarget, ID: r.PathValue("vid"), Content: r.PathValue("id")}
}
func groupTarget(r *http.Request) Target { return Target{Kind: GroupTarget, ID: r.PathValue("id")} }

func putOffer(target func(*http.Request) Target) func(handler, http.ResponseWriter, *http.Request) {
	return func(h handler, w http.ResponseWriter, r *http.Request) {
		t := target(r)
		var terms Terms
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&terms); err != nil {
			h.fail(w, r, fmt.Errorf("%w: %w", ErrInvalid, err))
			return
		}
		if err := h.s.Authorize(r.Context(), h.auth, gate.Actor(r.Context()), []Target{t}); err != nil {
			h.fail(w, r, err)
			return
		}
		out, err := h.s.Put(r.Context(), []Put{{Target: t, Terms: terms}})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, out[0])
	}
}

func withdrawOffer(target func(*http.Request) Target) func(handler, http.ResponseWriter, *http.Request) {
	return func(h handler, w http.ResponseWriter, r *http.Request) {
		t := target(r)
		if err := h.s.Authorize(r.Context(), h.auth, gate.Actor(r.Context()), []Target{t}); err != nil {
			h.fail(w, r, err)
			return
		}
		out, err := h.s.Withdraw(r.Context(), []Target{t})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, out[0])
	}
}

func (h handler) join(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.Join(r.Context(), gate.Actor(r.Context()), []string{r.PathValue("id")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, out[0])
}

func (h handler) leave(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.Leave(r.Context(), gate.Actor(r.Context()), []string{r.PathValue("id")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, out[0])
}

func (h handler) library(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := LibraryQuery{Language: q.Get("language"), Cursor: q.Get("cursor")}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			h.fail(w, r, fmt.Errorf("%w: limit", ErrInvalid))
			return
		}
		lq.Limit = n
	}
	page, err := h.s.Library(r.Context(), gate.Actor(r.Context()), lq)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, page)
}

func (h handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := ListQuery{State: State(q.Get("state")), Cursor: q.Get("cursor")}
	if lq.State != "" && lq.State != Active && lq.State != Withdrawn {
		h.fail(w, r, fmt.Errorf("%w: state", ErrInvalid))
		return
	}
	var err error
	if q.Has("unsynced") {
		if lq.Unsynced, err = strconv.ParseBool(q.Get("unsynced")); err != nil {
			h.fail(w, r, fmt.Errorf("%w: unsynced", ErrInvalid))
			return
		}
	}
	if q.Has("limit") {
		if lq.Limit, err = strconv.Atoi(q.Get("limit")); err != nil || lq.Limit < 1 {
			h.fail(w, r, fmt.Errorf("%w: limit", ErrInvalid))
			return
		}
	}
	page, err := h.s.List(r.Context(), lq)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, page)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (h handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := httpapi.CodeInternal, "internal error"
	switch {
	case errors.Is(err, ErrInvalid):
		code, msg = httpapi.CodeInvalidRequest, err.Error()
	case errors.Is(err, ErrNotFound):
		code, msg = httpapi.CodeNotFound, "not found"
	case errors.Is(err, ErrForbidden):
		code, msg = httpapi.CodeForbidden, "forbidden"
	case errors.Is(err, ErrNotJoinable):
		code, msg = codeNotJoinable, err.Error()
	case errors.Is(err, ErrConflict):
		code, msg = httpapi.CodeConflict, err.Error()
	case errors.Is(err, access.ErrUnavailable), errors.Is(err, auth.ErrUnavailable):
		code, msg = httpapi.CodeUnavailable, "billing or auth unavailable; retry"
	case errors.Is(err, auth.ErrUnauthenticated):
		code, msg = httpapi.CodeUnauthorized, "authentication required"
	}
	if code == httpapi.CodeInternal || code == httpapi.CodeUnavailable {
		h.s.log.ErrorContext(r.Context(), "offers: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	c, _ := httpapi.LookupErrorCode(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}
