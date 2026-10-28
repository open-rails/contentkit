package content

import (
	"context"
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
