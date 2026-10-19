package notification

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/helpers/notify"
)

// Handler serves the authenticated person's inbox and read state, and the
// host service's publish route. People select no recipient and write no
// notices.
func (s *Store) Handler(a auth.Authenticator) (http.Handler, error) {
	if err := gate.Check(a, access.NotificationsPublish); err != nil {
		return nil, fmt.Errorf("notification: Handler: %w", err)
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: a, Log: s.log}
	httpapi.Mount(mux, g.Guard, s, routes)
	return mux, nil
}

func init() { httpapi.Register(httpapi.Notifications, routes) }

var routes = []httpapi.Route[*Store]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/inbox", Resource: "notifications", Doc: "The signed-in person's inbox, newest first.", Auth: httpapi.User, Query: []httpapi.Param{httpapi.Int("limit", "page size: 20 by default, at most 100"), httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Bool("unread", "only unread notices"), httpapi.Bool("dismissed", "archived receipts instead of the active inbox")}, Responses: []httpapi.Reply{httpapi.OK(notify.Page{})}}, Serve: httpapi.H((*Store).handleList)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/{id}/read", Resource: "notifications", Doc: "Marks one of the signed-in person's notices read locally.", Auth: httpapi.User, Responses: []httpapi.Reply{httpapi.OK(notify.Item{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: func(s *Store) http.HandlerFunc { return s.handleRead(true) }},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/{id}/read", Resource: "notifications", Doc: "Marks one of the signed-in person's notices unread locally.", Auth: httpapi.User, Responses: []httpapi.Reply{httpapi.OK(notify.Item{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: func(s *Store) http.HandlerFunc { return s.handleRead(false) }},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/{id}/dismiss", Resource: "notifications", Doc: "Archives this receipt without changing its read or workflow state.", Auth: httpapi.User, Responses: []httpapi.Reply{httpapi.OK(notify.Item{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: func(s *Store) http.HandlerFunc { return s.handleDismiss(true) }},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/{id}/dismiss", Resource: "notifications", Doc: "Restores an archived receipt to this user's inbox.", Auth: httpapi.User, Responses: []httpapi.Reply{httpapi.OK(notify.Item{})}, Errors: []string{httpapi.CodeNotFound}}, Serve: func(s *Store) http.HandlerFunc { return s.handleDismiss(false) }},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/publish", Resource: "notifications", Doc: "Publishes notices in one transaction, each once per origin and recipient; a notice to an erased recipient is dropped.", Auth: httpapi.Staff, Perm: string(access.NotificationsPublish), Request: Publication{}, Responses: []httpapi.Reply{httpapi.OK(Published{})}, Errors: []string{httpapi.CodeConflict}}, Serve: httpapi.H((*Store).handlePublish)},
}

// MaxPublish bounds one publish batch.
const MaxPublish = 100

// Publication is a batch of notices a host service publishes.
type Publication struct {
	Notifications []notify.Input `json:"notifications"`
}

// Published is each notice of a Publication as stored, in order, without
// those dropped for an erased recipient.
type Published struct {
	Items []notify.Item `json:"items"`
}

// PublishBatch stores a batch in one transaction (see the publish route).
func (s *Store) PublishBatch(ctx context.Context, in []notify.Input) ([]notify.Item, error) {
	if len(in) == 0 || len(in) > MaxPublish {
		return nil, fmt.Errorf("%w: publish 1 to %d notices", notify.ErrInvalid, MaxPublish)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// Each notice locks its recipient; taking them in one order keeps two
	// concurrent batches from deadlocking. Results keep the caller's order.
	order := make([]int, len(in))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		ra, rb := in[a].Recipient, in[b].Recipient
		return cmp.Or(cmp.Compare(ra.Audience, rb.Audience), cmp.Compare(ra.ID, rb.ID))
	})
	stored := make([]*notify.Item, len(in))
	for _, i := range order {
		item, err := s.PublishTx(ctx, tx, in[i])
		switch {
		case errors.Is(err, notify.ErrRecipientErased):
			continue
		case err != nil:
			return nil, err
		}
		stored[i] = &item
	}
	out := make([]notify.Item, 0, len(in))
	for _, item := range stored {
		if item != nil {
			out = append(out, *item)
		}
	}
	return out, tx.Commit(ctx)
}

func (s *Store) handlePublish(w http.ResponseWriter, r *http.Request) {
	var in Publication
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		s.fail(w, r, notify.ErrInvalid)
		return
	}
	for i, n := range in.Notifications {
		if s.appID != "" && (n.Visibility == notify.VisibilityTenant || (n.AppID != "" && n.AppID != s.appID)) {
			s.fail(w, r, notify.ErrInvalid)
			return
		}
		if string(n.Data) == "null" { // a Go client's nil Data
			in.Notifications[i].Data = nil
		}
	}
	items, err := s.PublishBatch(r.Context(), in.Notifications)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(Published{Items: items})
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := notify.ListOptions{Cursor: q.Get("cursor")}
	var err error
	if q.Has("limit") {
		opts.Limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			s.fail(w, r, notify.ErrInvalid)
			return
		}
	}
	if q.Has("unread") {
		opts.UnreadOnly, err = strconv.ParseBool(q.Get("unread"))
		if err != nil {
			s.fail(w, r, notify.ErrInvalid)
			return
		}
	}
	if q.Has("dismissed") {
		opts.Dismissed, err = strconv.ParseBool(q.Get("dismissed"))
		if err != nil {
			s.fail(w, r, notify.ErrInvalid)
			return
		}
	}
	page, err := s.List(r.Context(), recipient(r), opts)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(page)
}
func (s *Store) handleRead(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, err := s.SetRead(r.Context(), recipient(r), r.PathValue("id"), read)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeItem(w, item)
	}
}
func (s *Store) handleDismiss(dismissed bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := recipient(r)
		id := r.PathValue("id")

		item, err := s.SetDismissed(r.Context(), target, id, dismissed)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeItem(w, item)
	}
}
func writeItem(w http.ResponseWriter, item notify.Item) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(item)
}
func (s *Store) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := http.StatusInternalServerError, httpapi.CodeInternal, "notifications unavailable"
	switch {
	case errors.Is(err, auth.ErrUnavailable):
		status, code, msg = http.StatusServiceUnavailable, httpapi.CodeUnavailable, "notifications unavailable"
	case errors.Is(err, notify.ErrInvalid):
		status, code, msg = http.StatusBadRequest, httpapi.CodeInvalidRequest, "invalid request"
	case errors.Is(err, notify.ErrConflict):
		status, code, msg = http.StatusConflict, httpapi.CodeConflict, "notification origin has conflicting visibility"
	case errors.Is(err, notify.ErrNotFound):
		status, code, msg = http.StatusNotFound, httpapi.CodeNotFound, "notification not found"
	default:
		s.log.ErrorContext(r.Context(), "notification request failed", "method", r.Method, "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func recipient(r *http.Request) notify.Recipient {
	return notify.Recipient{Audience: notify.AudienceUser, ID: gate.Actor(r.Context()).AccountID()}
}
