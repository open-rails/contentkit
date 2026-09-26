package contentkit

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/chmigrate"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/worker"
)

// RuntimeConfig configures one tenant's full ContentKit: the search and signal
// planes (EmbeddedConfig) and the interaction module (Content). Pool, tenant
// and schema are shared: Content.Pool, Content.Tenant and Content.Schema are
// filled from the hub configuration when empty. Posts join the keyword queue.
type RuntimeConfig struct {
	EmbeddedConfig
	Content content.Options
}

// Runtime is the one surface a host wires: the Hub (search, typeahead,
// signals, discovery) plus the content module and its HTTP routes.
type Runtime struct {
	*EmbeddedHub
	Content *content.Runtime
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
	rt, err := content.New(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Runtime{EmbeddedHub: hub, Content: rt}, nil
}

// Handler returns the content routes (comments, reactions, favorites, polls,
// posts). Mount it under a prefix after the host's auth middleware.
func (r *Runtime) Handler() http.Handler { return r.Content.Handler() }

// WorkerOptions returns the host's keyword worker options extended with
// ContentKit's own documents: posts (content.KindPost) are listed and built by
// the content module, every other kind by the host callbacks.
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
			return nil, "", true, nil
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
