package content

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// AccountLifecycle is the engine's account lifecycle, served to a host that
// delivers AuthKit's account events over HTTP.
type AccountLifecycle interface {
	SetAccountDeleted(ctx context.Context, accountID string, deleted bool) error
	AnonymizeAccount(ctx context.Context, accountID string) error
}

// AccountDeleted is PUT /v1/accounts/{id}/deleted's body.
type AccountDeleted struct {
	// Deleted marks the account deleted (AuthKit's recovery period) or, false,
	// restored.
	Deleted bool `json:"deleted"`
}

var accountRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/accounts/{id}/deleted", Resource: "accounts", Auth: httpapi.Staff, Perm: string(access.AccountsLifecycle),
		Doc:     "Marks an account deleted during AuthKit's recovery period, hiding its attribution, or restored. Idempotent.",
		Request: AccountDeleted{}, Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H((*Runtime).handleAccountDeleted)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/accounts/{id}/anonymize", Resource: "accounts", Auth: httpapi.Staff, Perm: string(access.AccountsLifecycle),
		Doc:       "Anonymizes a purged account: keeps its comments and contributions, unlinks it and removes its private data. Idempotent; retry on failure.",
		Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H((*Runtime).handleAnonymizeAccount)},
}

func (rt *Runtime) handleAccountDeleted(w http.ResponseWriter, req *http.Request) {
	var in AccountDeleted
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	rt.lifecycle(w, func(l AccountLifecycle) error {
		return l.SetAccountDeleted(req.Context(), req.PathValue("id"), in.Deleted)
	})
}

func (rt *Runtime) handleAnonymizeAccount(w http.ResponseWriter, req *http.Request) {
	rt.lifecycle(w, func(l AccountLifecycle) error { return l.AnonymizeAccount(req.Context(), req.PathValue("id")) })
}

func (rt *Runtime) lifecycle(w http.ResponseWriter, call func(AccountLifecycle) error) {
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
