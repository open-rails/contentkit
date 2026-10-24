package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// maxBody bounds one batch's JSON.
const maxBody = 64 << 20

func init() { httpapi.Register(httpapi.Import, routes) }

var routes = []httpapi.Route[*Service]{
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/import", Resource: "import", Auth: httpapi.Staff, Perm: string(access.ImportWrite),
		Doc: "Imports a batch of groups, content items, memberships, aliases, comments, reactions, favorites, polls and signals, idempotently by " +
			"external id: re-sending converges. Records that fail are reported and the rest written; their rebuild jobs are queued.",
		Request: Batch{}, Responses: []httpapi.Reply{httpapi.OK(Result{})}},
		Serve: httpapi.H((*Service).handleImport)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/import/external-ids", Resource: "import", Auth: httpapi.Staff, Perm: string(access.ImportWrite),
		Doc: "Pages the external ids imports under a source hold, per entity, with their ids and whether each is deleted: " +
			"a persistent sync compares them with its source to find the deletes to send.",
		Request: ExternalIDsQuery{}, Responses: []httpapi.Reply{httpapi.OK(ExternalIDs{})}},
		Serve: httpapi.H((*Service).handleExternalIDs)},
}

// Handler serves POST /v1/import and /v1/import/external-ids to holders of
// access.ImportWrite.
func (s *Service) Handler(a auth.Authenticator) (http.Handler, error) {
	if err := gate.Check(a, access.ImportWrite); err != nil {
		return nil, fmt.Errorf("importer: Handler: %w", err)
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: a, Log: s.o.Logger}
	httpapi.Mount(mux, g.Guard, s, routes)
	return mux, nil
}

func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) {
	var b Batch
	if !decode(w, r, &b) {
		return
	}
	res, err := s.Import(r.Context(), b)
	s.answer(w, r, res, err)
}

func (s *Service) handleExternalIDs(w http.ResponseWriter, r *http.Request) {
	var q ExternalIDsQuery
	if !decode(w, r, &q) {
		return
	}
	res, err := s.ExternalIDs(r.Context(), q)
	s.answer(w, r, res, err)
}

// decode reads one JSON value into v, or answers 400.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		reply(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, httpapi.BodyError(err))
		return false
	}
	if _, extra := dec.Token(); extra != io.EOF {
		reply(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, "the body holds more than one JSON value")
		return false
	}
	return true
}

func (s *Service) answer(w http.ResponseWriter, r *http.Request, res any, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		reply(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, httpapi.Rule(err, httpapi.CodeInvalidRequest))
	case err != nil:
		s.o.Logger.ErrorContext(r.Context(), "contentkit: import failed", "err", err)
		reply(w, http.StatusInternalServerError, httpapi.CodeInternal, "internal error")
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

func reply(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}
