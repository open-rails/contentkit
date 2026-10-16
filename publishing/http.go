package publishing

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
)

func init() { httpapi.Register(httpapi.Publishing, routes) }

var routes = []httpapi.Route[*Store]{
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/publishing-operations/{id}", Resource: "publishing", Auth: httpapi.Public,
		Doc:     "Creates or resumes one immutable archive submission. id is a client-chosen UUID. Requires content write; creating missing taxonomy also requires group write. Tenant and app come from the verified request. Repeating identical input returns the same draft and renews expired upload instructions; changed input is a conflict. when_ready requires provider-supported durable authorization.",
		Request: Request{}, Responses: []httpapi.Reply{httpapi.OK(Operation{})}, Errors: []string{"operation_conflict", "source_conflict", "ambiguous_entity", "unknown_entity", "operation_busy", httpapi.CodeForbidden, "not_configured"}}, Serve: httpapi.H((*Store).putHTTP)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/publishing-operations/{id}/complete", Resource: "publishing", Auth: httpapi.Public,
		Doc:     "Completes the submission's declared upload and durably begins processing. The server owns its exact media commit; no caller-supplied content IDs or blob references are accepted. Repeats do not create another import.",
		Request: CompleteRequest{}, Responses: []httpapi.Reply{httpapi.Accepted(Operation{})}, Errors: []string{httpapi.CodeNotFound, httpapi.CodeForbidden, "not_uploaded", "operation_busy"}}, Serve: httpapi.H((*Store).completeHTTP)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/publishing-operations/{id}", Resource: "publishing", Auth: httpapi.Public,
		Doc:       "Reads an owned submission, its compact progress, asynchronous failure or final catalog path. This read never creates upload URLs or advances the operation. HTTP 200 does not imply successful publication.",
		Responses: []httpapi.Reply{httpapi.OK(Operation{})}, Errors: []string{httpapi.CodeNotFound, httpapi.CodeForbidden}}, Serve: httpapi.H((*Store).getHTTP)},
}

func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: s.o.Auth, Log: s.o.Logger}
	httpapi.Mount(mux, g.Guard, s, routes)
	return mux
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return failure("invalid_request", "", "invalid JSON request")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return failure("invalid_request", "", "request must contain one JSON value")
	}
	return nil
}

func (s *Store) putHTTP(w http.ResponseWriter, r *http.Request) {
	var in Request
	if err := decode(r, &in); err != nil {
		s.respond(w, r, Operation{}, err, 0)
		return
	}
	out, err := s.Put(r.Context(), gate.Actor(r.Context()), r.PathValue("id"), in)
	s.respond(w, r, out, err, http.StatusOK)
}
func (s *Store) completeHTTP(w http.ResponseWriter, r *http.Request) {
	var in CompleteRequest
	if err := decode(r, &in); err != nil {
		s.respond(w, r, Operation{}, err, 0)
		return
	}
	out, err := s.Complete(r.Context(), gate.Actor(r.Context()), r.PathValue("id"))
	s.respond(w, r, out, err, http.StatusAccepted)
}
func (s *Store) getHTTP(w http.ResponseWriter, r *http.Request) {
	out, err := s.Get(r.Context(), gate.Actor(r.Context()), r.PathValue("id"))
	s.respond(w, r, out, err, http.StatusOK)
}

func (s *Store) respond(w http.ResponseWriter, r *http.Request, out Operation, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err == nil {
		if out.State == AwaitingUpload || out.State == Processing || out.State == Publishing {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	f := &OperationError{Code: "internal_error", Message: "internal error"}
	status = http.StatusInternalServerError
	var known *OperationError
	var ue *media.UploadError
	switch {
	case errors.As(err, &known):
		f = known
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrExpired), errors.Is(err, auth.ErrRevoked):
		f = failure("unauthorized", "", "authentication required")
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, catalog.ErrForbidden), errors.Is(err, groups.ErrForbidden):
		f = failure("forbidden", "", "permission denied")
	case errors.Is(err, auth.ErrUnavailable), errors.Is(err, media.ErrUnavailable):
		f = &OperationError{Code: "unavailable", Message: "dependency temporarily unavailable", Retryable: true}
	case errors.Is(err, media.ErrCommitPending):
		f = &OperationError{Code: "unavailable", Message: "the earlier commit is being recovered; retry", Retryable: true}
	case errors.Is(err, media.ErrCommitIdentity):
		f = failure("operation_conflict", "", "the upload commit does not match its original operation")
	case errors.Is(err, catalog.ErrInvalid), errors.Is(err, groups.ErrInvalid):
		f = failure("invalid_request", "", err.Error())
	case errors.Is(err, catalog.ErrConflict), errors.Is(err, groups.ErrConflict):
		f = failure("conflict", "", "conflicting content or metadata")
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, groups.ErrNotFound):
		f = failure("not_found", "", "not found")
	case errors.As(err, &ue):
		f = &OperationError{Code: ue.Code, Message: ue.Message, Retryable: ue.Code == media.CodeUnavailable || ue.Code == media.CodeNotUploaded || ue.Code == media.CodeNotFound || ue.Code == media.CodeIncomplete}
	}
	if code, ok := httpapi.LookupErrorCode(f.Code); ok {
		status = code.Status
	}
	if status >= 500 {
		s.o.Logger.ErrorContext(r.Context(), "publishing request failed", "error", err)
	}
	if f.Retryable {
		w.Header().Set("Retry-After", "2")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		Retryable bool   `json:"retryable,omitempty"`
		Field     string `json:"field,omitempty"`
	}{f.Message, f.Code, f.Retryable, f.Field})
}
