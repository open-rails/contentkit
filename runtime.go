package contentkit

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/chmigrate"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/notification"
)

// RuntimeConfig configures one tenant's full ContentKit: the search and signal
// planes (EmbeddedConfig), the interaction module (Content) and the modules
// Handler serves beside it. Pool, tenant and schema are shared:
// Content.Pool, Content.Tenant and Content.Schema are filled from the hub
// configuration when empty, and Content.Media.Reader from Reader. Posts join
// the keyword queue.
type RuntimeConfig struct {
	EmbeddedConfig
	// Auth says who a request is (authkit/auth; AuthKit's
	// ak.Authenticator()). Every module's routes are gated by it; ContentKit
	// decides what each admits and answers refusals itself. Required.
	Auth auth.Authenticator
	// UserInfo is the host's directory (authkit/userinfo; AuthKit's
	// ak.UserInfo()): authors' names and usernames.
	UserInfo userinfo.Lookup
	// Content is the interaction module; its Auth and UserInfo are the ones
	// above, so leave them unset.
	Content content.Options

	// Optional modules Handler serves, each at its fixed sub-path.
	Uploads *media.Uploads // the upload API at /media/upload
	Reader  *media.Reader  // the read API at /media
	// ReadLimit is the read API's per-viewer limit (media.HandlerOptions.Limit).
	ReadLimit media.RateLimit
	Codes     *contenturl.Router // GET /codes/{code}
	// Taxonomy serves the store's admin API at /taxonomy, for staff holding
	// access.TaxonomyWrite.
	Taxonomy TaxonomyAdmin
}

// TaxonomyAdmin is *taxonomy.Store: the admin API it serves.
type TaxonomyAdmin interface {
	Handler(a auth.Authenticator) (http.Handler, error)
}

// Runtime is the one surface a host wires: the Hub (search, typeahead,
// signals, discovery), the content module and the HTTP API of every
// configured module.
type Runtime struct {
	*EmbeddedHub
	Content *content.Runtime
	// Notifications is the tenant inbox. Only trusted host code creates notices.
	Notifications *notification.Store
	handler       http.Handler
}

// NewRuntime builds the hub and the content module over the host pool.
func NewRuntime(ctx context.Context, cfg RuntimeConfig) (*Runtime, error) {
	c := cfg.Content
	if c.Auth != nil || c.UserInfo != nil {
		return nil, fmt.Errorf("contentkit: set RuntimeConfig.Auth and UserInfo, not Content's")
	}
	c.Auth, c.UserInfo = cfg.Auth, cfg.UserInfo
	hub, err := NewEmbedded(cfg.EmbeddedConfig)
	if err != nil {
		return nil, err
	}
	if c.Pool == nil {
		c.Pool = cfg.PG
	}
	if strings.TrimSpace(c.Tenant) == "" {
		c.Tenant = hub.Tenant()
	}
	if c.Tenant != hub.Tenant() {
		return nil, fmt.Errorf("contentkit: content tenant %q differs from hub tenant %q", c.Tenant, hub.Tenant())
	}
	if c.Schema == "" {
		c.Schema = cfg.PGSchema
	}
	if c.Schema != cfg.PGSchema {
		return nil, fmt.Errorf("contentkit: content schema %q differs from PostgreSQL schema %q", c.Schema, cfg.PGSchema)
	}
	if c.Media != nil && c.Media.Reader == nil && cfg.Reader != nil {
		m := *c.Media
		m.Reader = cfg.Reader
		c.Media = &m
	}
	if cfg.Codes != nil && cfg.Codes.Tenant() != c.Tenant {
		return nil, fmt.Errorf("contentkit: codes tenant %q differs from content tenant %q", cfg.Codes.Tenant(), c.Tenant)
	}
	for _, kind := range c.ContentKinds {
		if slices.Contains(httpapi.Reserved(), kind) {
			return nil, fmt.Errorf("contentkit: content kind %q is a module's path under Runtime.Handler", kind)
		}
	}
	rt, err := content.New(ctx, c)
	if err != nil {
		return nil, err
	}
	notices := rt.Notifications
	h, err := handler(cfg, c, rt, notices)
	if err != nil {
		return nil, err
	}
	return &Runtime{EmbeddedHub: hub, Content: rt, Notifications: notices, handler: h}, nil
}

// Handler serves ContentKit's HTTP API: every configured module under one
// prefix, each at its fixed sub-path (docs/api/routes.md), each route behind
// its tier's gate over RuntimeConfig.Auth. Mount it once; no auth middleware
// goes in front:
//
//	mux.Handle("/api/contentkit/", http.StripPrefix("/api/contentkit", rt.Handler()))
//
// A host may instead mount the modules alone (content.Runtime.Handler,
// media.UploadHandler, media.Reader.Handler, contenturl.Router.Handler,
// taxonomy.Handler, notification.Store.Handler)
// at paths of its choosing.
func (r *Runtime) Handler() http.Handler { return r.handler }

func handler(cfg RuntimeConfig, c content.Options, rt *content.Runtime, notices *notification.Store) (http.Handler, error) {
	mux := http.NewServeMux()
	mount := func(m httpapi.Module, h http.Handler) {
		if p := m.Prefix(); p != "" {
			mux.Handle(p+"/", http.StripPrefix(p, h))
		}
	}
	mux.Handle("/", rt.Handler())
	nh, err := notices.Handler(c.Auth)
	if err != nil {
		return nil, err
	}
	mount(httpapi.Notifications, nh)
	if cfg.Uploads != nil {
		mount(httpapi.Upload, media.UploadHandler(cfg.Uploads, media.UploadHandlerOptions{Auth: c.Auth, Logger: c.Logger}))
	}
	if cfg.Reader != nil {
		mount(httpapi.Media, cfg.Reader.Handler(media.HandlerOptions{Auth: c.Auth, Logger: c.Logger, Limit: cfg.ReadLimit}))
	}
	if cfg.Codes != nil {
		mount(httpapi.Codes, cfg.Codes.Handler())
	}
	if cfg.Taxonomy != nil {
		h, err := cfg.Taxonomy.Handler(c.Auth)
		if err != nil {
			return nil, err
		}
		mount(httpapi.Taxonomy, h)
	}
	return mux, nil
}

// MigrateConfig selects the host-owned stores for ContentKit migrations.
type MigrateConfig struct {
	// DB holds PostgreSQL DDL credentials. Required.
	DB *sql.DB
	// Schema receives every PostgreSQL table; it may be the application's schema.
	// Required. Identifiers contain letters, numbers or underscores.
	Schema string
	// ClickHouse applies the signal baseline when set; PostgresDB defaults to DB.
	ClickHouse *chmigrate.Config
}

// Migrate applies PostgreSQL migrations in one host-selected schema and the
// optional ClickHouse signal baseline.
func Migrate(ctx context.Context, cfg MigrateConfig) error {
	if err := migrations.ApplyPostgres(ctx, cfg.DB, cfg.Schema); err != nil {
		return err
	}
	if cfg.ClickHouse == nil {
		return nil
	}
	ch := *cfg.ClickHouse
	if ch.App == "" {
		ch.App = "contentkit_signal"
	}
	if ch.PostgresDB == nil {
		ch.PostgresDB = cfg.DB
	}
	chmigs, err := migratekit.Load(migrations.ClickHouse, ".")
	if err != nil {
		return fmt.Errorf("contentkit: load signal migrations: %w", err)
	}
	m := chmigrate.New(&ch)
	defer m.Close()
	if err := m.ApplyMigrations(ctx, chmigs); err != nil {
		return fmt.Errorf("contentkit: apply signal migrations: %w", err)
	}
	return nil
}
