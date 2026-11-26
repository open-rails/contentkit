// Package contentkit is ContentKit running in this process: the content
// library a host configures and mounts, as it does OpenRails and AuthKit.
//
//	ck, err := contentkit.New(ctx, cfg, deps)  // validates or migrates its schema
//	err = ck.Start(ctx)                        // serve: enqueue jobs on its own River
//	err = ck.StartJobs(ctx)                    // in the process that works jobs
//	routes, err := ck.Routes(contentkit.Routes{Prefix: "/api/contentkit", Entities: contentkit.AllRoutes})
//
// The routes, the Go client (Client) and Ready, Probes and Close are the
// whole surface: hosts never read or write ContentKit's storage.
package contentkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/internal/activitylog"
	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	channelstore "github.com/open-rails/contentkit/internal/channelstore"
	contentstore "github.com/open-rails/contentkit/internal/contentstore"
	discoverystore "github.com/open-rails/contentkit/internal/discoverystore"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	entitystore "github.com/open-rails/contentkit/internal/entitystore"
	importstore "github.com/open-rails/contentkit/internal/importstore"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/internal/readingstore"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	urlstore "github.com/open-rails/contentkit/internal/urlstore"
	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/clientaddr"
	"github.com/open-rails/contentkit/internal/endpoint"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/locationpolicy"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/internal/migrations"
	"github.com/open-rails/contentkit/internal/rankings"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
)

// DefaultSchema is Config.Database.Schema's default.
const DefaultSchema = "contentkit"

var schemaRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Engine is ContentKit running in this process for one app's catalog.
type Engine struct {
	cfg             Config
	scope           *Scope
	erasureRecovery bool
	schema          string
	riverSchema     string
	pool            *pgxpool.Pool
	session         *pgxpool.Config // Deps.PostgresSession; nil when pool is direct
	locks           *pglock.Locks
	log             *slog.Logger
	auth            auth.Authenticator
	gate            *entitlement.Gate
	geo             *locationpolicy.Geo

	hub      *hub.Hub
	signals  *signalPlane // nil without ClickHouse
	reading  *readingstore.Store
	activity *signalstore.Writer // nil without ClickHouse
	now      func() time.Time
	alog     *activitylog.Log
	chMu     sync.Mutex
	ownCH    clickhouse.Conn // opened from Config.ClickHouse
	content  *contentstore.Runtime
	entities *entitystore.Store
	channels *channelstore.Store
	catalog  *catalogstore.Store
	versions *versionstore.Service
	discover *discoverystore.Service
	rankings *rankings.Engine
	importer *importstore.Service
	media    *engineMedia
	avatars  *engineAvatars
	modules  map[httpapi.Module]http.Handler
	inproc   http.Handler
	probes   []Probe

	mu      sync.Mutex
	jobs    *jobs
	river   *river.Client[pgx.Tx] // ContentKit's own, built by Start
	working bool                  // StartJobs started river
	stopRun context.CancelFunc
	started bool
	closed  bool
}

type engineMedia struct {
	polls    bool // the registry declares the poll kind
	versions *versionstore.Service
	reg      *mediastore.Registry
	store    media.Store
	cache    *mediastore.PGCache
	jobs     *mediastore.Jobs
	uploads  *mediastore.Uploads
	reader   *mediastore.Reader
	limit    mediastore.RateLimit
	queue    *workqueue.Queue
	progress mediastore.ProgressSource
}

// New runs ContentKit in this process. With Database.SkipMigrations it checks
// prepared catalog, shared account, notification and queue storage read-only;
// the host calls MigratePostgres and MigrateClickHouse explicitly beforehand.
// The zero value retains automatic embedded migrations for compatibility.
// A configured ClickHouse outage disables signals while the engine retries;
// read-only mode never applies DDL on those retries. New starts no jobs: Start
// does, and queued jobs wait until then. ctx bounds database initialization.
func New(ctx context.Context, cfg Config, deps Deps) (_ *Engine, err error) {
	return newEngine(ctx, cfg, deps, false)
}

// NewForErasureReplay opens the normal app stores for explicit restore repair.
// It permits pending erasure replay, but cannot serve routes, use Client, or
// start jobs. Close it after ReplayErasureFences and construct New normally.
func NewForErasureReplay(ctx context.Context, cfg Config, deps Deps) (*Engine, error) {
	cfg.Database.SkipMigrations = true
	return newEngine(ctx, cfg, deps, true)
}

func newEngine(ctx context.Context, cfg Config, deps Deps, recovery bool) (_ *Engine, err error) {
	e := &Engine{cfg: cfg, pool: deps.Postgres, auth: deps.Auth, log: deps.Logger, erasureRecovery: recovery}
	if e.log == nil {
		e.log = slog.Default()
	}
	now := deps.Clock
	if now == nil {
		now = time.Now
	}
	e.now = now
	if err := e.validate(deps); err != nil {
		return nil, err
	}
	cfg = e.cfg
	defer func() {
		if err != nil {
			_ = e.Close(context.WithoutCancel(ctx))
		}
	}()
	if e.cfg.Database.SkipMigrations {
		if err := e.validateDatabase(ctx); err != nil {
			return nil, err
		}
	} else if err := e.migrate(ctx); err != nil {
		return nil, err
	}
	if e.geo, err = locationpolicy.Open(cfg.GeoIPCityPath, e.log); err != nil {
		return nil, fmt.Errorf("contentkit: GeoIP City database: %w", err)
	}
	e.signals = e.newSignals(deps)
	if e.hub, err = hub.New(hub.Config{PG: e.pool, PGSchema: e.schema, Tenant: cfg.Scope.AppID.String(), DefaultLanguage: cfg.Content.Languages[0],
		SignalPlane: e.signals != nil}); err != nil {
		return nil, err
	}
	// Similar lists need only Postgres; rankings fill from the signal plane.
	if e.rankings, err = rankings.New(rankings.Options{Pool: e.pool, Schema: e.schema, Tenant: cfg.Scope.AppID.String(), Kinds: cfg.Content,
		Source: e.hub, Redis: deps.Redis, Logger: e.log, Now: now}); err != nil {
		return nil, err
	}
	if e.gate, err = entitlement.NewGate(entitlement.GateConfig{Tenant: cfg.Scope.AppID.String(), Kinds: cfg.Content, Billing: deps.Billing, Logger: e.log, Now: now}); err != nil {
		return nil, err
	}
	e.reading = readingstore.New(e.pool, e.schema, uuid.UUID(cfg.Scope.AppID), now)
	e.alog = &activitylog.Log{App: uuid.UUID(cfg.Scope.AppID), Kinds: cfg.Content, Gate: e.gate, Now: now}
	if e.signals != nil && !recovery {
		e.activity = signalstore.NewWriter(cfg.Scope.AppID.String(), e.hub.Signals, e.log)
		e.alog.Write = e.activity.Add
		e.activity.Start()
	}
	if err := e.newCatalog(ctx, deps, now); err != nil {
		return nil, err
	}
	if err := e.newVersions(deps, now); err != nil {
		return nil, err
	}
	if cfg.Media != nil {
		if e.media, err = e.newMedia(ctx, deps, now); err != nil {
			return nil, err
		}
		if e.avatars, err = e.newAvatars(deps); err != nil {
			return nil, err
		}
		if err := e.catalog.BindMedia(catalogstore.Media{Registry: e.media.reg, Reader: e.media.reader, Versions: catalogMedia{Service: e.media.versions, engine: e}}); err != nil {
			return nil, err
		}
	}
	if e.content, err = contentstore.New(ctx, e.contentOptions(deps)); err != nil {
		return nil, err
	}
	if err := e.mount(deps); err != nil {
		return nil, err
	}
	e.jobs = &jobs{e: e}
	e.probes = e.newProbes(deps)
	if e.signals != nil {
		if recovery {
			first, cancel := context.WithTimeout(ctx, e.signals.bootWait)
			e.signals.attempt(first)
			cancel()
		} else {
			e.signals.start(ctx)
		}
	}
	if !recovery {
		if err := e.checkErasureReplay(ctx); err != nil {
			return nil, err
		}
	}
	if recovery {
		e.inproc = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, ErrErasureReplayRequired.Error(), http.StatusServiceUnavailable)
		})
	}
	return e, nil
}

func (e *Engine) validate(deps Deps) error {
	if err := e.configureStorage(); err != nil {
		return err
	}
	cfg := &e.cfg
	var err error
	if err := clientaddr.CheckTrusted(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("contentkit: Config.TrustedProxies: %w", err)
	}
	if cfg.AnonymousRetention < 0 {
		return errors.New("contentkit: Config.AnonymousRetention is negative")
	}
	if cfg.AnonymousRetention == 0 {
		cfg.AnonymousRetention = DefaultAnonymousRetention
	}
	if err := cfg.Content.Validate(); err != nil {
		return fmt.Errorf("contentkit: Config.Content: %w", err)
	}
	if err := e.validateEntityScopes(deps.Scopes); err != nil {
		return err
	}
	for kind := range cfg.Content.Kinds {
		if slices.Contains(httpapi.Reserved(), kind) {
			return fmt.Errorf("contentkit: content kind %q is a route group's path", kind)
		}
	}
	if deps.Postgres == nil {
		return errors.New("contentkit: Deps.Postgres is required")
	}
	if s := deps.PostgresSession; s != nil {
		if s.ConnConfig == nil {
			return errors.New("contentkit: Deps.PostgresSession has no connection settings")
		}
		switch mode := deps.Postgres.Config().ConnConfig.DefaultQueryExecMode; mode {
		case pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol:
		default:
			// A transaction pooler may run a statement's prepare and execute
			// on different server connections.
			return fmt.Errorf("contentkit: Deps.Postgres goes through a transaction pooler (Deps.PostgresSession is set) and needs DefaultQueryExecMode cache_describe, exec or simple_protocol, not %s", mode)
		}
		e.session = s.Copy()
	}
	e.locks = pglock.New(e.sessionConfig())
	e.auth, err = e.authorization(deps.Auth, deps.Authorization)
	if err != nil {
		return err
	}
	if err := gate.Check(e.auth, access.Permissions()...); err != nil {
		return fmt.Errorf("contentkit: Deps.Auth: %w", err)
	}
	if cfg.Media != nil {
		if _, err := cfg.MediaKinds(); err != nil {
			return err
		}
		if deps.Store == nil {
			return errors.New("contentkit: Config.Media needs Deps.Store")
		}
	}
	if a := cfg.Accounts; a != nil && a.Avatars != nil {
		a := a.Avatars
		if cfg.Media == nil {
			return errors.New("contentkit: Config.Accounts.Avatars needs Config.Media")
		}
		if a.Primary != "" {
			if _, err := endpoint.Secure(a.Primary); err != nil {
				return fmt.Errorf("contentkit: Config.Accounts.Avatars.Primary: %w", err)
			}
			if strings.TrimSpace(a.Key) == "" {
				return errors.New("contentkit: Config.Accounts.Avatars.Primary needs Key")
			}
		}
	}
	if (deps.ClickHouse != nil || cfg.ClickHouse.Addr != "") && strings.TrimSpace(cfg.ClickHouse.Database) == "" {
		return errors.New("contentkit: the signal plane needs Config.ClickHouse.Database")
	}
	if _, err := cfg.ClickHouse.tlsConfig(); err != nil {
		return err
	}
	return nil
}

// migrate applies the catalog, account metadata, River and media worker
// migrations under their migrators' locks, then claims the configured scope.
func (e *Engine) migrate(ctx context.Context) error {
	return e.migratePostgres(ctx, MigrationOptions{})
}

// sessionConfig dials what needs a session of its own: Deps.PostgresSession,
// else the pool's configuration.
func (e *Engine) sessionConfig() *pgxpool.Config {
	if e.session != nil {
		return e.session
	}
	return e.pool.Config()
}

func (e *Engine) migratePostgres(ctx context.Context, options MigrationOptions) error {
	// The migrators hold session locks and pin connections: behind a pooler
	// they run on a direct pool of their own.
	direct := e.pool
	if e.session != nil {
		var err error
		if direct, err = pgxpool.NewWithConfig(ctx, e.session.Copy()); err != nil {
			return fmt.Errorf("contentkit: migration session: %w", err)
		}
		defer direct.Close()
	}
	db := stdlib.OpenDBFromPool(direct)
	defer db.Close()
	if err := migrations.ApplyPostgres(ctx, db, e.schema); err != nil {
		return err
	}
	accountSchema, _ := e.accountStorage()
	if err := migrations.ApplyAccounts(ctx, db, accountSchema); err != nil {
		return err
	}
	if err := e.bindScope(ctx); err != nil {
		return err
	}
	if err := readingstore.SeedOutcomeValues(ctx, e.pool, e.schema, uuid.UUID(e.cfg.Scope.AppID)); err != nil {
		return fmt.Errorf("contentkit: seed outcome values: %w", err)
	}
	if options.explicit {
		// A retry after a lost provider reply must converge here, not enqueue
		// redundant background provisioning for the reserved entity.
		if err := catalogstore.InitializeFixedOwners(ctx, e.pool, e.schema, e.cfg.Scope.AppID.String(), e.cfg.Content, options.Scopes); err != nil {
			return err
		}
	}
	if err := e.avatarPlacement(ctx, options.explicit); err != nil {
		return err
	}
	if options.SkipQueues {
		return nil
	}
	if err := riverhelpers.ApplyMigrations(ctx, direct, riverhelpers.MigrationOptions{Schema: e.riverSchema, Logger: e.log}); err != nil {
		return fmt.Errorf("contentkit: River migrations: %w", err)
	}
	if account, ok := e.accountMediaStorage(); ok {
		if err := workqueue.Migrate(ctx, direct, account.WorkerSchema); err != nil {
			return fmt.Errorf("contentkit: account media queue migrations: %w", err)
		}
	}
	if m := e.cfg.Media; m != nil {
		if m.WorkerSchema == "" {
			m.WorkerSchema = e.schema + "_media_worker"
		}
		if err := workqueue.Migrate(ctx, direct, m.WorkerSchema); err != nil {
			return fmt.Errorf("contentkit: media worker queue migrations: %w", err)
		}
	}
	return nil
}

// newCatalog builds the stores media is built over: entities and the catalog, which refreshes facets when entities change.
func (e *Engine) newCatalog(ctx context.Context, deps Deps, now func() time.Time) error {
	c := e.cfg.Content
	opts := entitystore.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Scope.AppID.String(), Kinds: c, Scopes: deps.Scopes, Logger: e.log,
		ContentChanged: func(ctx context.Context, q entitystore.Querier, ids []string) error {
			return e.catalog.ContentChanged(ctx, q, ids)
		},
		NamesChanged: func(ctx context.Context, q entitystore.Querier, ids []string) error {
			return e.catalog.EntitiesRenamed(ctx, q, ids)
		},
		DocumentsChanged: func(ctx context.Context, q entitystore.Querier) error { return e.syncDocuments(ctx, q) },
		EntitiesChanged: func(ctx context.Context, q entitystore.Querier, changes []entities.Change) error {
			return e.catalog.EntitiesChanged(ctx, q, changes)
		},
		Owned: ownedContent{e},
		Hidden: func(ctx context.Context) ([]string, error) {
			return e.catalog.HiddenEntities(ctx, gate.Actor(ctx), gate.Domain(ctx))
		}}
	if e.cfg.Media != nil {
		opts.Covers, opts.Folders = entityCovers{e}, entityFolders{e}
	}
	if e.hub.SignalsConfigured() {
		opts.Rankings = e.rankings
	}
	var err error
	opts.Gate = e.gate
	if e.entities, err = entitystore.New(opts); err != nil {
		return err
	}
	e.channels = channelstore.FromStore(e.entities)
	co := catalogstore.Options{Pool: e.pool, Locks: e.locks, Schema: e.schema, Tenant: e.cfg.Scope.AppID.String(), Kinds: c, Auth: e.auth, Gate: e.gate, Entities: e.entities, Channels: e.channels,
		Body: deps.BodySanitizer, Now: now, Logger: e.log,
		EventRetention: e.cfg.EventRetention, OnPurge: e.forgetPurgedTx, AfterPurge: e.purgeSignals,
		DocumentsChanged: func(ctx context.Context, q catalogstore.Querier) error { return e.syncDocuments(ctx, q) }}
	if e.catalog, err = catalogstore.New(ctx, co); err != nil {
		return err
	}
	return nil
}

// newVersions binds the one publication service before optional media storage.
func (e *Engine) newVersions(deps Deps, now func() time.Time) error {
	opts := versionstore.Options{Tenant: e.cfg.Scope.AppID.String(), Schema: e.schema, Pool: e.pool, Locks: e.locks,
		Kinds: e.cfg.Content, Auth: e.auth, AccountExists: e.accountExists(deps),
		Resolver: e.catalog.Resolver(), Gate: e.gate, Facets: e.catalog, Others: siteMedia{e},
		Policy: deps.Downloads, Logger: e.log, Now: now, MediaConfigured: e.cfg.Media != nil}
	if mc := e.cfg.Media; mc != nil {
		opts.Downloads, opts.Redis = mc.Downloads, deps.Redis
	}
	var err error
	if e.versions, err = versionstore.New(opts); err != nil {
		return err
	}
	return e.catalog.BindPublication(e.versions)
}

func (e *Engine) newMedia(ctx context.Context, deps Deps, now func() time.Time) (*engineMedia, error) {
	mc := e.cfg.Media
	reg, err := e.cfg.MediaKinds()
	if err != nil {
		return nil, err
	}
	m := &engineMedia{store: deps.Store, versions: e.versions}
	declared := map[string]bool{}
	for _, k := range reg.Kinds {
		declared[k.Name] = true
	}
	m.polls = declared[pollFolder]
	if m.reg, err = mediastore.NewRegistry(reg, m.versions.Hooks()); err != nil {
		return nil, err
	}
	accountStorage, sharedAccounts := e.accountMediaStorage()
	accountQueue := ""
	if sharedAccounts {
		accountQueue = accountStorage.WorkerSchema
	}
	if m.queue, err = workqueue.NewWithAccounts(e.pool, m.reg, mc.WorkerSchema, accountQueue); err != nil {
		return nil, err
	}
	// Every manifest write lands in the item cache, which wakes releases and
	// garbage collection on the engine's River schema.
	host, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: e.riverSchema})
	if err != nil {
		return nil, err
	}
	recorded, err := versionstore.Recorded(e.pool, e.locks, e.riverSchema, e.schema, m.reg)
	if err != nil {
		return nil, err
	}
	// Commits enqueue their own processing; the cache wakes releases and GC.
	cacheOptions := mediastore.CacheOptions{Lock: recorded.Lock, Recorded: recorded.Recorded, GC: mediastore.RiverGC{Client: host}, Now: now}
	if m.cache, err = mediastore.NewPGCache(e.pool, e.schema, m.reg, cacheOptions); err != nil {
		return nil, err
	}
	if sharedAccounts {
		account, err := mediastore.NewPGCache(e.pool, accountStorage.Schema, m.reg, cacheOptions)
		if err != nil {
			return nil, err
		}
		if m.cache, err = mediastore.RouteCache(m.cache, account, e.accountMediaPartition()); err != nil {
			return nil, err
		}
	}
	manifests, err := mediastore.NewManifests(deps.Store, m.reg, mediastore.ManifestOptions{Written: m.cache.Record, Current: m.cache.ETag, Now: now})
	if err != nil {
		return nil, err
	}
	if m.jobs, err = mediastore.NewJobs(mediastore.JobsConfig{Manifests: manifests, Processes: m.queue, Logger: e.log, Now: now}); err != nil {
		return nil, err
	}
	d := mc.Delivery
	signing, err := token.ParseKey(d.SigningKey)
	if err != nil {
		return nil, fmt.Errorf("contentkit: Config.Media.Delivery.SigningKey: %w", err)
	}
	var previous *token.Key
	if d.PreviousKey != "" {
		k, err := token.ParseKey(d.PreviousKey)
		if err != nil {
			return nil, fmt.Errorf("contentkit: Config.Media.Delivery.PreviousKey: %w", err)
		}
		previous = &k
	}
	ring, err := token.NewRing(signing, previous)
	if err != nil {
		return nil, err
	}
	rate := func(r RateLimitConfig) mediastore.RateLimit {
		return mediastore.RateLimit{PerSecond: r.PerSecond, Burst: r.Burst, Disabled: r.Disabled, Redis: deps.Redis}
	}
	if m.uploads, err = mediastore.NewUploads(mediastore.UploadOptions{Store: deps.Store, Manifests: m.jobs.Manifests(), Processes: m.queue, Tickets: &ring,
		ProcessOnUpload: mc.ProcessOnUpload, Commits: rate(mc.CommitLimit)}); err != nil {
		return nil, err
	}
	if m.progress, err = workqueue.NewProgressSourceWithAccounts(e.pool, m.reg, mc.WorkerSchema, accountQueue); err != nil {
		return nil, err
	}
	mode := media.DeliverURL
	if d.Mode != "" {
		mode = media.DeliveryMode(d.Mode)
	}
	if m.reader, err = mediastore.NewReader(mediastore.ReaderOptions{Manifests: m.jobs.Manifests(), Queue: m.queue, Progress: m.progress,
		Delivery: media.Delivery{Mode: mode, CookieDomain: d.CookieDomain, Keys: ring, TTL: d.TTL, Window: d.Window},
		Issuance: mediastore.Issuance{PerHour: mc.Issuance.PerHour, AnonymousPerHour: mc.Issuance.AnonymousPerHour, Disabled: mc.Issuance.Disabled, Redis: deps.Redis},
		Now:      now}); err != nil {
		return nil, err
	}
	m.limit = rate(mc.ReadLimit)
	if err := m.versions.Bind(versionstore.Media{Manifests: m.jobs.Manifests(), Cache: m.cache, Uploads: m.uploads, Jobs: m.jobs, Reader: m.reader}); err != nil {
		return nil, err
	}
	return m, nil
}

func (e *Engine) contentOptions(deps Deps) contentstore.Options {
	in := e.cfg.Interactions
	rates := func(rs []Rate) []content.Rate {
		var out []content.Rate
		for _, r := range rs {
			out = append(out, content.Rate{Count: r.Count, Per: r.Per})
		}
		return out
	}
	l := in.Limits
	o := contentstore.Options{
		Locate: e.geo.Lookup, Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Scope.AppID.String(), Auth: e.auth, UserInfo: deps.Users, Resolver: interactionTargets{e}, Kinds: e.cfg.Content,
		TextProcessor:         deps.TextProcessor,
		NotificationPublisher: deps.Notifications,
		Accounts:              e,
		Moderator:             deps.Moderator, ModeratorTimeout: in.ModeratorTimeout, ModeratorCooldown: in.ModeratorCooldown,
		ProviderDataEraser: deps.ProviderDataEraser,
		Activity:           e.alog,
		Touch: func(ctx context.Context, tx pgx.Tx, s uuid.UUID) error {
			return e.reading.Touch(ctx, tx, s)
		},
		Limits: contentstore.Limits{Disabled: l.Disabled, Redis: deps.Redis, Comment: rates(l.Comment), CommentReaction: rates(l.CommentReaction),
			Reaction: rates(l.Reaction), Favorite: rates(l.Favorite), PollVote: rates(l.PollVote)},
		Anonymous: in.Anonymous, CommentMaxLength: in.CommentMaxLength, DuplicateWindow: in.DuplicateWindow,
		Logger: e.log,
	}
	if e.media != nil {
		o.Media = &contentstore.Media{Namespace: e.media.reg.Namespace(), Images: e.media.cache, Folders: e.media.jobs, Reader: e.media.reader}
	}
	if e.avatars != nil && e.avatars.images != nil {
		o.Avatars = e.avatars.images
	}
	return o
}

// accountExists uses only the host's target-tenant directory. Source actor
// identity is not proof of target membership, including for global staff.
func (e *Engine) accountExists(deps Deps) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, id string) (bool, error) {
		if deps.Users == nil {
			return false, auth.ErrUnavailable
		}
		users, err := deps.Users.Get(ctx, []string{id})
		if err != nil {
			return false, err
		}
		user, found := users[id]
		return found && user.ID == id, nil
	}
}

// mount builds each configured module's handler and the in-process mux.
func (e *Engine) mount(deps Deps) error {
	var err error
	e.modules = map[httpapi.Module]http.Handler{httpapi.Content: e.content.Handler()}
	if m := e.media; m != nil {
		e.modules[httpapi.Upload] = mediastore.UploadHandler(m.uploads, mediastore.UploadHandlerOptions{Auth: e.auth, Logger: e.log})
		e.modules[httpapi.Media] = m.reader.Handler(mediastore.HandlerOptions{Auth: e.auth, Logger: e.log, Limit: m.limit, Events: e.mediaEvents})
		if a := e.avatars; a != nil && a.forward != nil {
			e.modules[httpapi.Upload] = a.forward.wrap(httpapi.Upload, e.modules[httpapi.Upload])
			e.modules[httpapi.Media] = a.forward.wrap(httpapi.Media, e.modules[httpapi.Media])
		}
		if e.modules[httpapi.Versions], err = m.versions.Handler(e.auth, e.log); err != nil {
			return err
		}
	}
	c := e.cfg.Content
	if e.modules[httpapi.Entities], err = e.entities.Handler(e.auth); err != nil {
		return err
	}
	if e.cfg.Content.Channels != nil {
		if e.modules[httpapi.Channels], err = e.channels.Handler(e.auth); err != nil {
			return err
		}
	}
	e.modules[httpapi.Contents] = e.catalog.Handler()
	if err := e.mountDiscovery(deps); err != nil {
		return err
	}
	avatars := func(context.Context) *media.PresetRule { return nil }
	if e.avatars != nil {
		avatars = e.avatars.rule
	}
	e.modules[httpapi.Kinds] = kindsHandler((&gate.Gate{Auth: e.auth, Log: e.log}).Guard, c, e.hub.SignalsConfigured, avatars)
	if e.importer, err = importstore.New(importstore.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Scope.AppID.String(), Catalog: e.catalog, Entities: e.entities, Channels: e.channels,
		Content: e.content, Signals: e.discover, Rebuild: e.rebuild, Logger: e.log}); err != nil {
		return err
	}
	if e.modules[httpapi.Import], err = e.importer.Handler(e.auth); err != nil {
		return err
	}
	urls, err := urlstore.New(urlstore.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Scope.AppID.String()})
	if err != nil {
		return err
	}
	router, err := urlstore.NewRouter(urls, contenturl.RouterOptions{Routes: c.Routes(), Languages: c.Languages, Logger: e.log,
		Visibility: e.codeVisibility})
	if err != nil {
		return err
	}
	e.modules[httpapi.Codes] = router.Handler((&gate.Gate{Auth: e.auth, Log: e.log}).Guard)
	e.inproc = e.dispatch("", e.modules, nil)
	return nil
}

// mountDiscovery builds the public read routes over the catalog's rule.
func (e *Engine) mountDiscovery(deps Deps) error {
	o := discoverystore.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Scope.AppID.String(), Kinds: e.cfg.Content, Resolver: e.catalog.Resolver(), Gate: e.gate,
		Entities: e.entities, Channels: e.channels, Hub: e.hub, Logger: e.log, Now: deps.Clock, Visibility: e.catalog, Reading: e.reading, Activity: e.alog,
		Feedback: e.cfg.Interactions.Feedback, Rankings: e.rankings}
	o.Tenure, _ = deps.Billing.(access.TenureReader)
	if e.media != nil {
		o.Media = e.media.versions
	}
	var err error
	if e.discover, err = discoverystore.New(o); err != nil {
		return err
	}
	e.catalog.BindStanding(e.discover)
	e.modules[httpapi.Discovery], err = e.discover.Handler(e.auth)
	return err
}

// Start makes the engine ready to serve. It builds ContentKit's own River
// client on Deps.Postgres in Config.Database.RiverSchema, through which
// requests enqueue background work, and works none of it: StartJobs does,
// in the process the host runs jobs in. It runs no DDL; ctx ending stops
// nothing. Call it once, before serving.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.start(ctx)
}

func (e *Engine) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case e.erasureRecovery:
		return ErrErasureReplayRequired
	case e.closed:
		return errors.New("contentkit: engine closed")
	case e.started:
		return errors.New("contentkit: Start called twice")
	}
	client, err := riverhelpers.NewWithSession(ctx, e.pool, e.session, &river.Config{Schema: e.riverSchema, Queues: map[string]river.QueueConfig{}, Logger: e.log, ReindexerSchedule: river.NeverSchedule()},
		e.jobs.contribution())
	if err != nil {
		return err
	}
	e.river, e.started = client, true
	return nil
}

// StartJobs works ContentKit's background jobs in this process: search
// indexing, the analytics job, entity popularity, publication, retention
// and media release work, and their schedules. A host calls it from the process
// that runs jobs (its worker command); processes that only serve call Start.
// It starts the engine first when Start has not run. Close stops the jobs.
func (e *Engine) StartJobs(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		if err := e.start(ctx); err != nil {
			return err
		}
	} else if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case e.closed:
		return errors.New("contentkit: engine closed")
	case e.working:
		return errors.New("contentkit: StartJobs called twice")
	}
	// The caller bounds startup, not the lifetime of the successfully started
	// workers. Keep cancellation connected only until startup returns; Close
	// owns the detached runtime and still allows graceful job completion.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopStartupCancellation := context.AfterFunc(ctx, cancel)
	err := e.river.Start(runCtx)
	if !stopStartupCancellation() {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		return errors.Join(fmt.Errorf("contentkit: start River: %w", err), e.river.StopAndCancel(cleanupCtx))
	}
	e.working, e.stopRun = true, cancel
	return nil
}

// Close stops the jobs StartJobs started and closes what New opened, leaving
// the host's pool, Redis and ClickHouse connections open.
func (e *Engine) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.activity != nil {
		// Write what requests queued while ClickHouse can still take it.
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := e.activity.Close(flush); err != nil {
			e.log.Warn("contentkit: activity not written at close", "err", err)
		}
		cancel()
	}
	if e.signals != nil {
		e.signals.close()
	}
	var errs []error
	if e.geo != nil {
		errs = append(errs, e.geo.Close())
	}
	if e.working {
		if err := e.river.Stop(ctx); err != nil {
			hard, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			errs = append(errs, err, e.river.StopAndCancel(hard))
			cancel()
		}
	}
	if e.stopRun != nil {
		e.stopRun()
	}
	e.chMu.Lock()
	if e.ownCH != nil {
		errs = append(errs, e.ownCH.Close())
	}
	e.chMu.Unlock()
	return errors.Join(errs...)
}

// Ready reports whether the engine can serve: Postgres answers and Start
// ran. Optional dependencies never fail it (see Probes).
func (e *Engine) Ready(ctx context.Context) error {
	if err := e.pool.Ping(ctx); err != nil {
		return fmt.Errorf("contentkit: postgres: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed:
		return errors.New("contentkit: engine closed")
	case !e.started:
		return errors.New("contentkit: not started")
	}
	return nil
}

// Probe checks one optional dependency.
type Probe struct {
	// Name is stable for dashboards: contentkit_clickhouse,
	// contentkit_media_store, contentkit_redis, contentkit_moderator.
	Name string
	// Check returns nil while the dependency is usable; it honors ctx.
	Check func(context.Context) error
}

// Probes lists the optional dependencies the engine degrades without, for the
// host's dependency supervisor: the signal plane, the media bucket, Redis and
// the moderator (failing while its breaker holds writes for review). Scoped
// engines also report account purges from the last 30 days stuck beyond the
// event reader's 15-minute grace period.
func (e *Engine) Probes() []Probe { return slices.Clone(e.probes) }

func (e *Engine) newProbes(deps Deps) []Probe {
	var out []Probe
	out = append(out, Probe{Name: "contentkit_account_anonymization", Check: e.checkAccountAnonymization})
	if e.signals != nil {
		out = append(out, Probe{Name: "contentkit_clickhouse", Check: e.signals.check})
	}
	if e.media != nil {
		store := e.media.store
		out = append(out, Probe{Name: "contentkit_media_store", Check: func(ctx context.Context) error {
			return store.Check(ctx)
		}})
	}
	if deps.Redis != nil {
		r := deps.Redis
		out = append(out, Probe{Name: "contentkit_redis", Check: func(ctx context.Context) error { return r.Ping(ctx).Err() }})
	}
	if deps.Moderator != nil {
		out = append(out, Probe{Name: "contentkit_moderator", Check: e.content.CheckModerator})
	}
	return out
}

// eraseAvatars permanently fences and verifies each tenant account avatar.
// Completion means the first storage cleanup finished, not just job acceptance.
func (e *Engine) eraseAvatars(ctx context.Context, users []string) error {
	if len(users) > 0 {
		schema, _ := e.accountStorage()
		var primary string
		err := e.pool.QueryRow(ctx, `SELECT primary_app_id::text FROM `+pgx.Identifier{schema, "content_account_avatar_placement"}.Sanitize()+` WHERE tenant_id=$1`, e.scope.TenantID.String()).Scan(&primary)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if primary == e.scope.AppID.String() && (e.media == nil || e.media.reg.Config().Avatars.Kind == "") {
			return errors.New("contentkit: avatar primary cannot complete account erasure without its avatar storage")
		}
	}
	if e.media == nil || len(users) == 0 {
		return nil
	}
	kind := e.media.reg.Config().Avatars.Kind
	if kind == "" {
		return nil
	}
	for _, u := range users {
		ref, err := e.media.reg.Ref(kind, u)
		if err != nil {
			return err
		}
		if err = e.media.jobs.Erase(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}
