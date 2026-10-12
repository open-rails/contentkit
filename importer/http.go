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
		Doc: "Imports a batch of groups, content items, memberships, offers, aliases, comments, reactions, favorites, polls and signals, idempotently by " +
			"external id: re-sending converges. Records that fail are reported and the rest written; their rebuild jobs are queued.",
		Request: Batch{}, Responses: []httpapi.Reply{httpapi.OK(Result{})}},
		Serve: httpapi.H((*Service).handleImport)},
}

// Handler serves POST /v1/import to holders of access.ImportWrite.
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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&b)
	if err == nil {
		if _, extra := dec.Token(); extra != io.EOF {
			err = errors.New("multiple JSON values")
		}
	}
	if err != nil {
		reply(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, "body: "+err.Error())
		return
	}
	res, err := s.Import(r.Context(), b)
	switch {
	case errors.Is(err, ErrInvalid):
		reply(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error())
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
