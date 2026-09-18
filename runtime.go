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
// and keyword schema are shared: Content.Pool, Content.Tenant and
// Content.SearchSchema are filled from the hub configuration when empty.
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
	if strings.TrimSpace(c.SearchSchema) == "" {
		c.SearchSchema = cfg.PGSchema
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

// MigrateConfig names the stores contentkit.Migrate applies the lineages to.
type MigrateConfig struct {
	// DB holds DDL credentials for the host Postgres. Required.
	DB *sql.DB
	// Schema is the host schema: the social lineage lands here. Required.
	Schema string
	// SearchSchema receives the keyword lineage; "" = Schema.
	SearchSchema string
	// SearchApp is the keyword lineage's ledger label; "" = "contentkit".
	// Existing keyword installations keep the label they were created with.
	SearchApp string
	// LegacySearch applies migrations.LegacyPostgres (existing installations
	// created from the pre-ContentKit combined lineage; docs/migration.md).
	LegacySearch bool
	// Taxonomy applies the optional catalog lineage in SearchSchema after the
	// keyword lineage it depends on. Its ledger app is contentkit_taxonomy.
	Taxonomy bool
	// ClickHouse applies the signal lineage when set; PostgresDB defaults to DB.
	ClickHouse *chmigrate.Config
}

// Migrate applies every ContentKit lineage: social in Schema, keyword in
// SearchSchema, optional taxonomy in SearchSchema, and the configured signal plane.
func Migrate(ctx context.Context, cfg MigrateConfig) error {
	if cfg.DB == nil {
		return fmt.Errorf("contentkit: DB is required")
	}
	if strings.TrimSpace(cfg.Schema) == "" {
		return fmt.Errorf("contentkit: Schema is required")
	}
	if err := content.Migrate(ctx, cfg.DB, cfg.Schema); err != nil {
		return err
	}
	searchSchema, app, lineage := cfg.SearchSchema, cfg.SearchApp, migrations.Postgres
	if searchSchema == "" {
		searchSchema = cfg.Schema
	}
	if app == "" {
		app = "contentkit"
	}
	if cfg.LegacySearch {
		lineage = migrations.LegacyPostgres
	}
	qs, err := search.QuoteSchema(searchSchema)
	if err != nil {
		return err
	}
	if _, err := cfg.DB.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+qs); err != nil {
		return fmt.Errorf("contentkit: ensure search schema: %w", err)
	}
	migs, err := migratekit.Load(lineage, ".", migratekit.RequireParentLinks())
	if err != nil {
		return fmt.Errorf("contentkit: load keyword migrations: %w", err)
	}
	if err := migratekit.NewPostgres(cfg.DB, app).WithSchema(searchSchema).ApplyMigrations(ctx, migs); err != nil {
		return fmt.Errorf("contentkit: apply keyword migrations: %w", err)
	}
	if cfg.Taxonomy {
		catalog, err := migratekit.Load(migrations.Taxonomy, ".", migratekit.RequireParentLinks())
		if err != nil {
			return fmt.Errorf("contentkit: load taxonomy migrations: %w", err)
		}
		if err := migratekit.NewPostgres(cfg.DB, "contentkit_taxonomy").WithSchema(searchSchema).ApplyMigrations(ctx, catalog); err != nil {
			return fmt.Errorf("contentkit: apply taxonomy migrations: %w", err)
		}
	}
	if cfg.ClickHouse == nil {
		return nil
	}
	ch := *cfg.ClickHouse
	if ch.PostgresDB == nil {
		ch.PostgresDB = cfg.DB
	}
	chmigs, err := migratekit.Load(migrations.SignalClickHouse, ".", migratekit.RequireParentLinks())
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
