// Package notification adapts the shared helpers/notify inbox to ContentKit's
// authenticated user API. Persistence and lifecycle belong to helpers/notify;
// hosts own rendering and application-specific actions.
package notification

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/notify/postgres"
)

// Config shares an account inbox without sharing a site's content catalog.
// Sites must share verified recipient IDs and the same Postgres database.
type Config struct {
	Schema        string               `json:"schema" yaml:"schema"`
	Namespace     string               `json:"namespace" yaml:"namespace"`
	SharedOrigins []notify.OriginScope `json:"shared_origins,omitempty" yaml:"shared_origins,omitempty"`
}

// Options selects the pool, ContentKit's schema and its tenant.
type Options struct {
	Pool           *pgxpool.Pool
	Schema, Tenant string
	Logger         *slog.Logger
	Shared         *Config
	// AppID comes from the engine immutable Scope, never from an HTTP selector.
	AppID string
}

// Store mounts a shared notification store as one ContentKit tenant's inbox.
// Embedded methods accept trusted host inputs; HTTP derives the user identity.
type Store struct {
	*postgres.Store
	pool              *pgxpool.Pool
	log               *slog.Logger
	schema, namespace string
	appID             string
	origins           []notify.OriginScope
	nativeOrigin      notify.OriginScope
	mu                sync.Mutex
	view              atomic.Pointer[postgres.Store]
}

// New constructs the adapter over tables contentkit.New installs.
func New(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Tenant) == "" {
		return nil, fmt.Errorf("notification: Tenant is required")
	}
	if opts.AppID != "" {
		id, err := uuid.Parse(opts.AppID)
		if err != nil || id == uuid.Nil || id.String() != opts.AppID {
			return nil, fmt.Errorf("notification: app ID must be a nonzero canonical UUID")
		}
	}
	schema, namespace := opts.Schema, opts.Tenant
	var origins []notify.OriginScope
	if opts.Shared != nil {
		if strings.TrimSpace(opts.Shared.Schema) == "" || strings.TrimSpace(opts.Shared.Namespace) == "" {
			return nil, fmt.Errorf("notification: shared schema and namespace required")
		}
		schema, namespace = opts.Shared.Schema, opts.Shared.Namespace
		origins = append([]notify.OriginScope{{System: "contentkit", Scope: opts.Tenant}}, opts.Shared.SharedOrigins...)
	}
	if opts.AppID != "" && origins == nil {
		origins = []notify.OriginScope{{System: "contentkit", Scope: opts.Tenant}}
	}
	store, err := postgres.New(opts.Pool, postgres.Config{Schema: schema, Scope: "contentkit:" + namespace})
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Store{Store: store, pool: opts.Pool, log: log, schema: schema, namespace: namespace, origins: origins, appID: opts.AppID, nativeOrigin: notify.OriginScope{System: "contentkit", Scope: opts.Tenant}}
	if err := s.setView(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) setView() error {
	config := postgres.Config{Schema: s.schema, Scope: "contentkit:" + s.namespace, Origins: s.origins}
	if s.appID != "" {
		config.Origins = nil
		config.AppView = &notify.AppView{AppID: s.appID, LegacyOrigins: s.origins}
	}
	v, err := postgres.New(s.pool, config)
	if err == nil {
		s.view.Store(v)
	}
	return err
}

// AllowOrigin adds one trusted producer namespace to a shared site's view.
// Called by integrations during startup; it grants no recipient authority.
func (s *Store) AllowOrigin(origin notify.OriginScope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.origins == nil {
		return nil
	}
	prior := s.origins
	s.origins = append(append([]notify.OriginScope{}, prior...), origin)
	if err := s.setView(); err != nil {
		s.origins = prior
		return err
	}
	return nil
}

// Namespace is the stable shared account namespace, or the legacy tenant.
func (s *Store) Namespace() string { return s.namespace }
func (s *Store) List(ctx context.Context, r notify.Recipient, o notify.ListOptions) (notify.Page, error) {
	return s.view.Load().List(ctx, r, o)
}
func (s *Store) Get(ctx context.Context, r notify.Recipient, id string) (notify.Item, error) {
	return s.view.Load().Get(ctx, r, id)
}
func (s *Store) SetRead(ctx context.Context, r notify.Recipient, id string, read bool) (notify.Item, error) {
	return s.view.Load().SetRead(ctx, r, id, read)
}
func (s *Store) SetDismissed(ctx context.Context, r notify.Recipient, id string, dismissed bool) (notify.Item, error) {
	return s.view.Load().SetDismissed(ctx, r, id, dismissed)
}

// AppID is the fixed app presentation boundary, empty only on legacy runtimes.
func (s *Store) AppID() string { return s.appID }

func (s *Store) publication(in notify.Input) (notify.Input, error) {
	if s.appID != "" {
		if in.Visibility == "" {
			in.Visibility = notify.VisibilityApp
			in.AppID = s.appID
		}
		if in.Visibility == notify.VisibilityApp && in.AppID != s.appID {
			return in, fmt.Errorf("%w: publication belongs to another app", notify.ErrInvalid)
		}
	}
	return in, nil
}

// Publish defaults native/host authored notices to this app. Tenant-wide
// publication is reserved for trusted in-process integrations; the HTTP route
// does not accept that authority from an app client.
func (s *Store) Publish(ctx context.Context, in notify.Input) (notify.Item, error) {
	in, err := s.publication(in)
	if err != nil {
		return notify.Item{}, err
	}
	return s.view.Load().Publish(ctx, in)
}
func (s *Store) PublishTx(ctx context.Context, tx pgx.Tx, in notify.Input) (notify.Item, error) {
	in, err := s.publication(in)
	if err != nil {
		return notify.Item{}, err
	}
	return s.view.Load().PublishTx(ctx, tx, in)
}
