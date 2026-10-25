// Package legacyinbox is the explicit one-time migration adapter for receipts
// created by older ContentKit releases. AuthKit owns canonical persistence;
// this package starts no runtime and mounts no user or publisher API.
package legacyinbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/authkit/notify/postgres"
)

// Config shares an account inbox without sharing a site's content catalog.
// Sites must share verified recipient IDs and the same Postgres database.
type Config struct {
	Schema        string               `json:"schema" yaml:"schema"`
	Namespace     string               `json:"namespace" yaml:"namespace"`
	SharedOrigins []notify.OriginScope `json:"shared_origins,omitempty" yaml:"shared_origins,omitempty"`
}

// ErrRecipientNotFound means the requested user is not a live account in this
// tenant. Consumers may skip the notice; a directory outage is a different,
// retryable error. No account or erasure tombstone is created by this result.
var ErrRecipientNotFound = errors.New("notification: recipient is not a live account in this tenant")

// Options selects the pool, ContentKit's schema and its tenant.
type Options struct {
	Pool           *pgxpool.Pool
	Schema, Tenant string
	Logger         *slog.Logger
	Shared         *Config
	// AppID comes from the engine immutable Scope, never from an HTTP selector.
	AppID string
	// AccountExists proves a user recipient belongs to this store's account
	// tenant. Scoped user publication fails closed without this lookup. The
	// directory must be fixed to the target tenant, not the request actor's.
	AccountExists func(context.Context, string) (bool, error)
	// AccountExistsTx provides the same proof on the caller's transaction.
	// PublishTx and batch publication never acquire a second pool connection.
	AccountExistsTx func(context.Context, pgx.Tx, string) (bool, error)
}

// Store mounts a shared notification store as one ContentKit tenant's inbox.
// Embedded methods accept trusted host inputs; HTTP derives the user identity.
type Store struct {
	*postgres.Store
	pool              *pgxpool.Pool
	log               *slog.Logger
	schema, namespace string
	appID             string
	accountExists     func(context.Context, string) (bool, error)
	accountExistsTx   func(context.Context, pgx.Tx, string) (bool, error)
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
	s := &Store{Store: store, pool: opts.Pool, log: log, schema: schema, namespace: namespace, origins: origins, appID: opts.AppID, accountExists: opts.AccountExists, accountExistsTx: opts.AccountExistsTx, nativeOrigin: notify.OriginScope{System: "contentkit", Scope: opts.Tenant}}
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

func (s *Store) checkRecipient(ctx context.Context, tx pgx.Tx, recipient notify.Recipient) error {
	if s.appID == "" || recipient.Audience != notify.AudienceUser {
		return nil
	}
	if tx != nil && s.accountExistsTx == nil || tx == nil && s.accountExists == nil {
		return fmt.Errorf("notification: scoped account lookup missing: %w", auth.ErrUnavailable)
	}
	var exists bool
	var err error
	if tx != nil {
		exists, err = s.accountExistsTx(ctx, tx, recipient.ID)
	} else {
		exists, err = s.accountExists(ctx, recipient.ID)
	}
	if err != nil {
		return fmt.Errorf("notification: recipient lookup: %w", errors.Join(auth.ErrUnavailable, err))
	}
	if !exists {
		return errors.Join(ErrRecipientNotFound, notify.ErrInvalid)
	}
	return nil
}

// Publish defaults native/host authored notices to this app. Tenant-wide
// publication is reserved for trusted in-process integrations; the HTTP route
// does not accept that authority from an app client.
func (s *Store) Publish(ctx context.Context, in notify.Input) (notify.Item, error) {
	in, err := s.publication(in)
	if err != nil {
		return notify.Item{}, err
	}
	if err := s.checkRecipient(ctx, nil, in.Recipient); err != nil {
		return notify.Item{}, err
	}
	return s.view.Load().Publish(ctx, in)
}
func (s *Store) PublishTx(ctx context.Context, tx pgx.Tx, in notify.Input) (notify.Item, error) {
	in, err := s.publication(in)
	if err != nil {
		return notify.Item{}, err
	}
	if err := s.checkRecipient(ctx, tx, in.Recipient); err != nil {
		return notify.Item{}, err
	}
	return s.view.Load().PublishTx(ctx, tx, in)
}

// Apply joins the legacy local store during explicit account-inbox adoption.
// New hosts inject AuthKit's publisher into ContentKit instead.
func (s *Store) Apply(ctx context.Context, mutations []notify.Mutation) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return s.ApplyTx(ctx, tx, mutations) })
}

func (s *Store) ApplyTx(ctx context.Context, tx pgx.Tx, mutations []notify.Mutation) error {
	if len(mutations) > notify.MaxMutations {
		return fmt.Errorf("%w: too many notification mutations", notify.ErrInvalid)
	}
	prepared := make([]notify.Mutation, 0, len(mutations))
	for _, mutation := range mutations {
		var err error
		mutation.Input, err = s.publication(mutation.Input)
		if err != nil {
			return err
		}
		if err := s.checkRecipient(ctx, tx, mutation.Recipient); errors.Is(err, ErrRecipientNotFound) {
			continue
		} else if err != nil {
			return err
		}
		prepared = append(prepared, mutation)
	}
	// A savepoint per recipient lets terminal recipient erasure drop that
	// recipient's notice without dropping another recipient's mutation.
	slices.SortStableFunc(prepared, func(a, b notify.Mutation) int {
		if d := strings.Compare(string(a.Recipient.Audience), string(b.Recipient.Audience)); d != 0 {
			return d
		}
		return strings.Compare(a.Recipient.ID, b.Recipient.ID)
	})
	return pgx.BeginFunc(ctx, tx, func(batch pgx.Tx) error {
		for _, mutation := range prepared {
			if err := s.view.Load().ApplyTx(ctx, batch, []notify.Mutation{mutation}); err != nil && !errors.Is(err, notify.ErrRecipientErased) {
				return err
			}
		}
		return nil
	})
}
