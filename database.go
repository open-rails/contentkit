package contentkit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/chmigrate"

	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/signal"
)

// ErrMigrationsRequired means a read-only startup check found storage that the
// host's explicit migration command must prepare before serving this version.
var ErrMigrationsRequired = errors.New("contentkit: database migrations required")

// configureStorage resolves only deployment identity and storage names. It is
// shared by runtime startup and explicit migration commands; it needs no auth,
// object store, billing client or content-model runtime.
func (e *Engine) configureStorage() error {
	resolved, scope, err := e.cfg.withScope()
	if err != nil {
		return err
	}
	resolved, err = resolved.withAccounts()
	if err != nil {
		return err
	}
	e.cfg, e.scope = resolved, scope
	c := &e.cfg
	c.Tenant = strings.TrimSpace(c.Tenant)
	if c.Tenant == "" {
		return errors.New("contentkit: Config.Tenant or Scope is required")
	}
	e.schema = strings.TrimSpace(c.Database.Schema)
	if e.schema == "" {
		e.schema = DefaultSchema
	}
	e.riverSchema = strings.TrimSpace(c.Database.RiverSchema)
	if e.riverSchema == "" {
		e.riverSchema = e.schema + "_river"
	}
	for _, schema := range []string{e.schema, e.riverSchema} {
		if !schemaRE.MatchString(schema) || len(schema) > 63 {
			return fmt.Errorf("contentkit: schema %q is not a lower-case identifier", schema)
		}
	}
	if n := c.Accounts; n != nil {
		if !schemaRE.MatchString(n.Schema) || len(n.Schema) > 63 || strings.TrimSpace(n.Namespace) == "" {
			return errors.New("contentkit: accounts require a valid schema and namespace")
		}
	}
	if c.Media != nil {
		media := *c.Media
		c.Media = &media
		if media.WorkerSchema == "" {
			media.WorkerSchema = e.schema + "_media_worker"
		}
		if err := workqueue.ValidSchema(media.WorkerSchema); err != nil {
			return err
		}
	}
	return nil
}

// MigrationOptions controls the explicit migration command. Runtime startup
// validation is unaffected and still requires every configured schema.
type MigrationOptions struct {
	// Scopes is the same app-bound provider used by Deps.Scopes. It is required
	// only when a configured fixed owner has auth_scope enabled. Migration
	// ensures those groups without changing their existing role assignments.
	Scopes entities.ScopeProvisioner
	// SkipQueues skips main River and media-worker queue migrations, matching
	// a host command's --skip-river option. Account/catalog migrations still run.
	SkipQueues bool
	explicit   bool
}

// MigratePostgres prepares all configured ContentKit PostgreSQL storage and
// records the configured scope binding. Call it from a host migration command,
// before New with Database.SkipMigrations=true. The flag never skips this
// explicit operation. Configured fixed owners and their authorization groups are
// provisioned using options.Scopes; no runtime or object-store client is started.
// The caller retains ownership of pool.
// When adopting an existing account inbox, run authkit.MigrateNotifications
// first. ContentKit validates and adopts that storage but never migrates it.
func MigratePostgres(ctx context.Context, pool *pgxpool.Pool, cfg Config, options ...MigrationOptions) error {
	if ctx == nil {
		return errors.New("contentkit: migration context is required")
	}
	if pool == nil {
		return errors.New("contentkit: migration pool is required")
	}
	if len(options) > 1 {
		return errors.New("contentkit: at most one MigrationOptions value is supported")
	}
	var opt MigrationOptions
	if len(options) == 1 {
		opt = options[0]
	}
	e := &Engine{cfg: cfg, pool: pool}
	if err := e.configureStorage(); err != nil {
		return err
	}
	for _, kind := range e.cfg.Content.Kinds {
		if kind.Channel == nil || kind.Channel.Fixed == "" {
			continue
		}
		if err := e.cfg.Content.Validate(); err != nil {
			return err
		}

		if opt.Scopes == nil {
			return errors.New("contentkit: configured auth_scope owner requires MigrationOptions.Scopes")
		}
		if err := e.validateScopeBinding(opt.Scopes); err != nil {
			return err
		}
	}
	opt.explicit = true
	return e.migratePostgres(ctx, opt)
}

// MigrateClickHouse creates the configured signal database before applying its
// schema. pool is PostgreSQL's migration-ledger pool, not a ClickHouse runtime
// connection. It is a no-op when ClickHouse is entirely unconfigured.
func MigrateClickHouse(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if ctx == nil {
		return errors.New("contentkit: migration context is required")
	}
	c := cfg.ClickHouse
	if c.Addr == "" && c.Database == "" {
		return nil
	}
	if pool == nil || strings.TrimSpace(c.Addr) == "" || strings.TrimSpace(c.Database) == "" {
		return errors.New("contentkit: ClickHouse migrations require a PostgreSQL ledger pool, address and database")
	}
	admin, err := clickhouse.Open(&clickhouse.Options{Addr: []string{c.Addr}, Auth: clickhouse.Auth{Database: "default", Username: c.Username, Password: c.Password}})
	if err != nil {
		return fmt.Errorf("contentkit: open ClickHouse migration connection: %w", err)
	}
	defer admin.Close()
	if err := signal.CreateDatabase(ctx, admin, c.Database, c.Cluster); err != nil {
		return err
	}
	steps, err := migratekit.Load(migrations.ClickHouse, ".")
	if err != nil {
		return fmt.Errorf("contentkit: load signal migrations: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	m := chmigrate.New(&chmigrate.Config{ClientAddr: c.Addr, Database: c.Database, Username: c.Username, Password: c.Password, Cluster: c.Cluster, App: "contentkit_signal", PostgresDB: db})
	defer m.Close()
	if err := m.ApplyMigrations(ctx, steps); err != nil {
		return fmt.Errorf("contentkit: apply signal migrations: %w", err)
	}
	if err := signal.CheckSchema(ctx, admin, c.Database); err != nil {
		return fmt.Errorf("contentkit: validate migrated signal schema: %w", err)
	}
	return nil
}

func (e *Engine) validateDatabase(ctx context.Context) error {
	required := migrationValidationError
	db := stdlib.OpenDBFromPool(e.pool)
	defer db.Close()
	if err := migrations.ValidatePostgres(ctx, db, e.schema); err != nil {
		return required(err)
	}
	accountSchema, _ := e.accountStorage()
	if err := migrations.ValidateAccounts(ctx, db, accountSchema); err != nil {
		return required(err)
	}
	if err := riverhelpers.ValidateMigrations(ctx, e.pool, e.riverSchema); err != nil {
		return required(err)
	}
	if account, ok := e.accountMediaStorage(); ok {
		if err := workqueue.Validate(ctx, e.pool, account.WorkerSchema); err != nil {
			return required(err)
		}
	}
	if m := e.cfg.Media; m != nil {
		if err := workqueue.Validate(ctx, e.pool, m.WorkerSchema); err != nil {
			return required(err)
		}
	}
	if err := e.avatarPlacement(ctx, false); err != nil {
		if errors.Is(err, ErrAvatarPlacement) {
			return err
		}
		return required(err)
	}
	if err := e.checkScope(ctx); err != nil {
		if errors.Is(err, ErrScopeConflict) {
			return err
		}
		return required(err)
	}
	return nil
}

// cleanupStorage resolves only the registry and journal routes; migration and
// startup validation must not need an object-store connection or live workers.
func (e *Engine) cleanupStorage() (*media.Registry, *media.PGJournal, error) {
	cfg, err := e.cfg.MediaKinds()
	if err != nil {
		return nil, nil, err
	}
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		return nil, nil, err
	}
	journal, err := media.NewPGJournal(e.pool, e.schema, nil)
	if err != nil {
		return nil, nil, err
	}
	if account, ok := e.accountMediaStorage(); ok {
		shared, err := media.NewPGJournal(e.pool, account.Schema, nil)
		if err != nil {
			return nil, nil, err
		}
		journal, err = media.RouteJournal(journal, shared, e.accountMediaPartition())
		if err != nil {
			return nil, nil, err
		}
	}
	return reg, journal, nil
}

// Connectivity, cancellation, resource pressure and server shutdown are retryable
// validation failures; they are not evidence that migrations are missing.
func migrationValidationError(err error) error {
	var network net.Error
	var connect *pgconn.ConnectError
	var pg *pgconn.PgError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) || errors.As(err, &connect) || errors.As(err, &pg) && (strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "53") || strings.HasPrefix(pg.Code, "57")) {
		return fmt.Errorf("contentkit: validate database migrations: %w", err)
	}
	return fmt.Errorf("%w; run the host migration command (contentkit.MigratePostgres): %w", ErrMigrationsRequired, err)
}
