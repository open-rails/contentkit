package channels

import (
	"encoding/json"
	"errors"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

func (s *Store) Handler(a auth.Authenticator) (http.Handler, error) {
	if err := gate.Check(a, access.ChannelsWrite); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: a, Log: slog.Default()}
	httpapi.Mount(mux, g.Guard, handler{s, a}, routes)
	return mux, nil
}
func init() { httpapi.Register(httpapi.Channels, routes) }

type handler struct {
	s    *Store
	auth auth.Authenticator
}

func (h handler) as(r *http.Request) *Store { return h.s.As(h.auth, gate.Actor(r.Context())) }

var listQuery = []httpapi.Param{httpapi.Text("language", "display language"), httpapi.Text("slug", "canonical channel slug"), httpapi.Text("q", "substring of a channel name or slug"), httpapi.Text("cursor", "previous next_cursor"), httpapi.Int("limit", "maximum results, at most 200"), httpapi.Text("sort", "name, count or newest")}
var routes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage/channels", Resource: "channels", Auth: httpapi.Staff, Perm: string(access.ChannelsWrite), Doc: "Lists channel administration records, including deleted channels during retention.", Query: append(append([]httpapi.Param{}, listQuery...), httpapi.Text("state", "active or deleted")), Responses: []httpapi.Reply{httpapi.OK(Page{})}}, Serve: httpapi.H(handler.manageList)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/channels", Resource: "channels", Auth: httpapi.Public, Doc: "Lists public channels independently of metadata entities.", Query: listQuery, Responses: []httpapi.Reply{httpapi.OK(Page{})}}, Serve: httpapi.H(handler.list)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/channels/{id}", Resource: "channels", Auth: httpapi.Public, Doc: "Reads a channel, its metadata and the viewer's membership standing.", Query: []httpapi.Param{httpapi.Text("language", "display language")}, Responses: []httpapi.Reply{httpapi.OK(Channel{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: httpapi.H(handler.get)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage/channels/{id}", Resource: "channels", Auth: httpapi.Staff, Perm: string(access.ChannelsWrite), Doc: "Reads channel administration identity, including a deleted channel during retention.", Query: []httpapi.Param{httpapi.Text("language", "display language")}, Responses: []httpapi.Reply{httpapi.OK(Channel{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: httpapi.H(handler.manage)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/channels", Resource: "channels", Auth: httpapi.User, Doc: "Lists the caller's channels and their current authoring permissions.", Query: listQuery, Responses: []httpapi.Reply{httpapi.OK(MyPage{})}}, Serve: httpapi.H(handler.mine)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/channels", Resource: "channels", Auth: httpapi.Editor, Perm: string(access.ChannelsWrite), Doc: "Creates a channel under the configured staff or user creation policy.", Request: Input{}, Responses: []httpapi.Reply{httpapi.Created(Channel{})}, Errors: []string{httpapi.CodeConflict, httpapi.CodeLimitReached}}, Serve: httpapi.H(handler.create)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/channels/{id}", Resource: "channels", Auth: httpapi.Editor, Perm: string(access.ChannelsWrite), Doc: "Edits channel identity as staff or a current channel manager.", Request: Update{}, Responses: []httpapi.Reply{httpapi.OK(Channel{})}, Errors: []string{httpapi.CodeNotFound, httpapi.CodeConflict}}, Serve: httpapi.H(handler.update)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/channels/{id}", Resource: "channels", Auth: httpapi.Editor, Perm: string(access.ChannelsWrite), Doc: "Soft-deletes the channel using its existing content-retention lifecycle.", Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{httpapi.CodeNotFound}}, Serve: httpapi.H(handler.remove)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/channels/{id}/entities", Resource: "channels", Auth: httpapi.Public, Doc: "Lists descriptive channel metadata; these associations confer no access or authoring authority.", Query: []httpapi.Param{httpapi.Text("language", "display language")}, Responses: []httpapi.Reply{httpapi.OK([]entities.EntityCard{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: httpapi.H(handler.metadata)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/channels/{id}/entities", Resource: "channels", Auth: httpapi.Editor, Perm: string(access.ChannelsWrite), Doc: "Replaces channel metadata with the given entity IDs, as a channel manager.", Request: []string{}, Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{httpapi.CodeNotFound, httpapi.CodeConflict}}, Serve: httpapi.H(handler.setMetadata)},
}

func opts(r *http.Request) ListOptions {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return ListOptions{Language: q.Get("language"), Slug: q.Get("slug"), Query: q.Get("q"), Cursor: q.Get("cursor"), Limit: limit, Sort: entities.Sort(q.Get("sort")), State: q.Get("state")}
}
func (h handler) list(w http.ResponseWriter, r *http.Request) {
	v, e := h.as(r).List(r.Context(), opts(r))
	reply(w, 200, v, e)
}
func (h handler) mine(w http.ResponseWriter, r *http.Request) {
	v, e := h.as(r).Mine(r.Context(), opts(r))
	reply(w, 200, v, e)
}
func (h handler) get(w http.ResponseWriter, r *http.Request) {
	v, e := h.as(r).Get(r.Context(), r.PathValue("id"), r.URL.Query().Get("language"))
	reply(w, 200, v, e)
}
func (h handler) manage(w http.ResponseWriter, r *http.Request) {
	v, e := h.as(r).ManagementGet(r.Context(), r.PathValue("id"), r.URL.Query().Get("language"))
	reply(w, 200, v, e)
}
func (h handler) create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if e := decode(w, r, &in); e != nil {
		reply(w, 0, nil, e)
		return
	}
	v, e := h.as(r).Create(r.Context(), in)
	reply(w, 201, v, e)
}
func (h handler) update(w http.ResponseWriter, r *http.Request) {
	var in Update
	if e := decode(w, r, &in); e != nil {
		reply(w, 0, nil, e)
		return
	}
	v, e := h.as(r).Update(r.Context(), r.PathValue("id"), in)
	reply(w, 200, v, e)
}
func (h handler) remove(w http.ResponseWriter, r *http.Request) {
	reply(w, 204, nil, h.as(r).Delete(r.Context(), r.PathValue("id")))
}
func (h handler) metadata(w http.ResponseWriter, r *http.Request) {
	v, e := h.as(r).Metadata(r.Context(), r.PathValue("id"), r.URL.Query().Get("language"))
	reply(w, 200, v, e)
}
func (h handler) setMetadata(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if e := decode(w, r, &ids); e != nil {
		reply(w, 0, nil, e)
		return
	}
	reply(w, 204, nil, h.as(r).SetMetadata(r.Context(), r.PathValue("id"), ids))
}
func decode(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.Join(entities.ErrInvalid, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return entities.ErrInvalid
	}
	return nil
}
func reply(w http.ResponseWriter, status int, out any, err error) {
	if err != nil {
		code := httpapi.CodeInternal
		status = 500
		switch {
		case errors.Is(err, entities.ErrInvalid):
			status, code = 400, httpapi.CodeInvalidRequest
		case errors.Is(err, entities.ErrNotFound):
			status, code = 404, httpapi.CodeNotFound
		case errors.Is(err, entities.ErrForbidden):
			status, code = 403, httpapi.CodeForbidden
		case errors.Is(err, entities.ErrConflict):
			status, code = 409, httpapi.CodeConflict
		case errors.Is(err, entities.ErrLimit):
			status, code = 409, httpapi.CodeLimitReached
		case errors.Is(err, auth.ErrUnavailable), errors.Is(err, access.ErrUnavailable):
			status, code = 503, httpapi.CodeUnavailable
		}
		if status == 500 {
			slog.Error("channel request failed", "err", err)
		}
		out = map[string]string{"error": httpapi.Message(code), "code": code}
	}
	if status == 204 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func (h handler) manageList(w http.ResponseWriter, r *http.Request) {
	v, e := h.as(r).ManagementList(r.Context(), opts(r))
	reply(w, 200, v, e)
}
