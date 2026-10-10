package contentkit

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/chmigrate"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/worker"
)

// RuntimeConfig configures one tenant's full ContentKit: the search and signal
// planes (EmbeddedConfig), the interaction module (Content) and the modules
// Handler serves beside it. Pool, tenant and schema are shared:
// Content.Pool, Content.Tenant and Content.Schema are filled from the hub
// configuration when empty. Posts join the keyword queue.
type RuntimeConfig struct {
	EmbeddedConfig
	Content content.Options

	// Optional modules Handler serves, each at its fixed sub-path. They
	// identify the caller with Content.Identity.
	Uploads *media.Uploads // the upload API at /media/upload
	Reader  *media.Reader  // the read API at /media
	// ReadLimit is the read API's per-viewer limit (media.HandlerOptions.Limit).
	ReadLimit media.RateLimit
	Codes     *contenturl.Router // GET /codes/{code}
	// Taxonomy is taxonomy.Handler(store): the admin API at /taxonomy, for
	// actors holding Content.Perms.Taxonomy.
	Taxonomy http.Handler
}

// Runtime is the one surface a host wires: the Hub (search, typeahead,
// signals, discovery), the content module and the HTTP API of every
// configured module.
type Runtime struct {
	*EmbeddedHub
	Content *content.Runtime
	handler http.Handler
}

// NewRuntime builds the hub and the content module over the host pool.
func NewRuntime(ctx context.Context, cfg RuntimeConfig) (*Runtime, error) {
	hub, err := NewEmbedded(cfg.EmbeddedConfig)
	if err != nil {
		return nil, err
	}
	c := cfg.Content
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
	return &Runtime{EmbeddedHub: hub, Content: rt, handler: handler(cfg, c, rt)}, nil
}

// Handler serves ContentKit's HTTP API: every configured module under one
// prefix, each at its fixed sub-path (docs/api/routes.md). Mount it once,
// after the host's auth middleware put the actor in the context:
//
//	mux.Handle("/api/contentkit/", http.StripPrefix("/api/contentkit", rt.Handler()))
//
// A host may instead mount the modules alone (content.Runtime.Handler,
// media.UploadHandler, media.Reader.Handler, contenturl.Router.Handler,
// taxonomy.Handler) at paths of its choosing.
func (r *Runtime) Handler() http.Handler { return r.handler }

func handler(cfg RuntimeConfig, c content.Options, rt *content.Runtime) http.Handler {
	mux := http.NewServeMux()
	mount := func(m httpapi.Module, h http.Handler) {
		if p := m.Prefix(); p != "" {
			mux.Handle(p+"/", http.StripPrefix(p, h))
		}
	}
	mux.Handle("/", rt.Handler())
	if cfg.Uploads != nil {
		mount(httpapi.Upload, media.UploadHandler(cfg.Uploads, media.UploadHandlerOptions{Logger: c.Logger,
			Actor: func(r *http.Request) (access.Actor, bool) {
				a, ok := c.Identity.Actor(r.Context())
				return a, ok && !a.Anonymous && a.ID != ""
			}}))
	}
	if cfg.Reader != nil {
		mount(httpapi.Media, cfg.Reader.Handler(media.HandlerOptions{Identity: c.Identity, Logger: c.Logger, Limit: cfg.ReadLimit}))
	}
	if cfg.Codes != nil {
		mount(httpapi.Codes, cfg.Codes.Handler())
	}
	if cfg.Taxonomy != nil {
		mount(httpapi.Taxonomy, rt.Guard(c.Perms.Taxonomy, cfg.Taxonomy))
	}
	return mux
}

// WorkerOptions returns the host's keyword worker options extended with
// ContentKit's own documents: posts (content.KindPost) are listed and built by
// the content module, every other kind by the host callbacks. Missing host
// callbacks for configured kinds fail backfill.
func (r *Runtime) WorkerOptions(host worker.Options) worker.Options {
	out := host
	if out.Pool == nil {
		out.Pool = r.client.pool
	}
	if out.Schema == "" {
		out.Schema = r.client.schema
	}
	if out.Tenant == "" {
		out.Tenant = r.tenant
	}
	hasPost := false
	for _, k := range out.ContentKinds {
		hasPost = hasPost || k == content.KindPost
	}
	if !hasPost {
		out.ContentKinds = append(append([]string(nil), out.ContentKinds...), content.KindPost)
	}
	list, build := host.ListContent, host.BuildKeywordDocuments
	out.ListContent = func(ctx context.Context, tenant, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
		if kind == content.KindPost {
			return r.Content.ListContent(ctx, tenant, kind, language, cursor, limit)
		}
		if list == nil {
			return nil, "", false, fmt.Errorf("contentkit: no ListContent for kind %q", kind)
		}
		return list(ctx, tenant, kind, language, cursor, limit)
	}
	out.BuildKeywordDocuments = func(ctx context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		if kind == content.KindPost {
			return r.Content.KeywordDocuments(ctx, tenant, kind, language, refs)
		}
		if build == nil {
			return nil, fmt.Errorf("contentkit: no BuildKeywordDocuments for kind %q", kind)
		}
		return build(ctx, tenant, kind, language, refs)
	}
	return out
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
	chmigs, err := migratekit.Load(migrations.ClickHouse, ".", migratekit.RequireParentLinks())
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
