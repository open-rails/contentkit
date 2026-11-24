package content

import (
	"context"
	"time"
)

// AccountLifecycle is the engine's account lifecycle, served to a host that
// delivers AuthKit's account events over HTTP.
type AccountLifecycle interface {
	SetAccountDeleted(ctx context.Context, accountID string, deleted bool) error
	AnonymizeAccount(ctx context.Context, accountID string) error
	AccountErasure(ctx context.Context, accountID string) (AccountErasure, error)
}

// AccountErasure is GET /v1/accounts/{id}/erasure: one account's lifecycle in
// this app. An erasure is pending while RequestedAt is set and CompletedAt is
// not; retry AnonymizeAccount until it completes.
type AccountErasure struct {
	AccountID string `json:"account_id"`
	// State is active, deleted (AuthKit's recovery period), purging
	// (anonymization started) or purged.
	State       string     `json:"state"`
	RequestedAt *time.Time `json:"requested_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

const (
	AccountStateActive = "active"
)

const (
	AccountStateDeleted = "deleted"
)

const (
	AccountStatePurging = "purging"
)

const (
	AccountStatePurged = "purged"
)

// AccountDeleted is PUT /v1/accounts/{id}/deleted's body.
type AccountDeleted struct {
	// Deleted marks the account deleted (AuthKit's recovery period) or, false,
	// restored.
	Deleted bool `json:"deleted"`
}
