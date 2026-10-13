// Package notification adapts the shared helpers/notify inbox to ContentKit's
// authenticated user API. Persistence and lifecycle belong to helpers/notify;
// hosts own rendering and application-specific actions.
package notification

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/notify/postgres"
)

// Options selects the pool, ContentKit's schema and its tenant.
type Options struct {
	Pool           *pgxpool.Pool
	Schema, Tenant string
	Logger         *slog.Logger
}

// Store mounts a shared notification store as one ContentKit tenant's inbox.
// Embedded methods accept trusted host inputs; HTTP derives the user identity.
type Store struct {
	*postgres.Store
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New constructs the adapter over tables contentkit.New installs.
func New(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Tenant) == "" {
		return nil, fmt.Errorf("notification: Tenant is required")
	}
	store, err := postgres.New(opts.Pool, postgres.Config{Schema: opts.Schema, Scope: "contentkit:" + opts.Tenant})
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Store{Store: store, pool: opts.Pool, log: log}, nil
}
