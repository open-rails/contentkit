package contentstore

import (
	"errors"
	"net/http"

	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
)

var accountRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/accounts/{id}/deleted", Resource: "accounts", Auth: httpapi.Staff, Perm: string(access.AccountsLifecycle),
		Doc:     "Marks an account deleted during AuthKit's recovery period, hiding its attribution, or restored. Idempotent.",
		Request: content.AccountDeleted{}, Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H((*Runtime).handleAccountDeleted)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/accounts/{id}/anonymize", Resource: "accounts", Auth: httpapi.Staff, Perm: string(access.AccountsLifecycle),
		Doc:       "Anonymizes a purged account: keeps its comments and contributions, unlinks it and removes its private data. Idempotent; retry on failure.",
		Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H((*Runtime).handleAnonymizeAccount)},
}

func (rt *Runtime) handleAccountDeleted(w http.ResponseWriter, req *http.Request) {
	var in content.AccountDeleted
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	rt.lifecycle(w, func(l content.AccountLifecycle) error {
		return l.SetAccountDeleted(req.Context(), req.PathValue("id"), in.Deleted)
	})
}

func (rt *Runtime) handleAnonymizeAccount(w http.ResponseWriter, req *http.Request) {
	rt.lifecycle(w, func(l content.AccountLifecycle) error { return l.AnonymizeAccount(req.Context(), req.PathValue("id")) })
}

func (rt *Runtime) lifecycle(w http.ResponseWriter, call func(content.AccountLifecycle) error) {
	if rt.accounts == nil {
		writeErr(w, errors.New("content: account lifecycle is not configured"))
		return
	}
	if err := call(rt.accounts); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
