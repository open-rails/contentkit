// Package contentkit is ContentKit running in this process: the content
// library a host configures and mounts, as it does OpenRails and AuthKit.
//
//	ck, err := contentkit.New(ctx, cfg, deps)  // validates or migrates its schema
//	err = ck.Start(ctx)                        // its jobs, on its own River
//	routes, err := ck.Routes(contentkit.Routes{Prefix: "/api/contentkit", Groups: contentkit.AllRoutes})
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
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/kindsroute"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/contentkit/offers"
	"github.com/open-rails/contentkit/publishing"
	"github.com/open-rails/contentkit/signal"
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
	log             *slog.Logger
	auth            auth.Authenticator
	gate            *access.Gate
	classifier      bool // free-text answers to reclassify

	hub              *hub.Hub
	signals          *signalPlane // nil without ClickHouse
	chMu             sync.Mutex
	ownCH            clickhouse.Conn // opened from Config.ClickHouse
	content          *content.Runtime
	notices          *notification.Store
	notificationJobs *billingNotificationJobs
	groups           *groups.Store
	catalog          *catalog.Store
	offers           *offers.Service
	discover         *discovery.Service
	importer         *importer.Service
	publishing       *publishing.Store
	media            *engineMedia
	avatars          *engineAvatars
	modules          map[httpapi.Module]http.Handler
	inproc           http.Handler
	probes           []Probe

	mu        sync.Mutex
	jobs      *jobs
	requested bool // RiverJobs was handed to a host fleet
	fleet     *river.Client[pgx.Tx]
	own       *river.Client[pgx.Tx]
	started   bool
	closed    bool
}

type engineMedia struct {
	polls    bool // the registry declares the poll kind
	versions *versions.Service
	reg      *media.Registry
	store    media.Store
	jobs     *media.Jobs
	uploads  *media.Uploads
	reader   *media.Reader
	limit    media.RateLimit
	queue    *workqueue.Queue
	progress media.ProgressSource
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
	e := &Engine{cfg: cfg, pool: deps.Postgres, auth: deps.Auth, log: deps.Logger, classifier: deps.Classifier != nil, erasureRecovery: recovery}
	if e.log == nil {
		e.log = slog.Default()
	}
	now := deps.Clock
	if now == nil {
		now = time.Now
	}
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
		if err := e.resolveAccountKey(ctx); err != nil {
			return nil, migrationValidationError(err)
		}
		if err := e.validateDatabase(ctx); err != nil {
			return nil, err
		}
	} else if err := e.migrate(ctx); err != nil {
		return nil, err
	}
	e.signals = e.newSignals(deps)
	if e.hub, err = hub.New(hub.Config{PG: e.pool, PGSchema: e.schema, Tenant: cfg.Tenant, DefaultLanguage: cfg.Content.Languages[0],
		SignalPlane: e.signals != nil}); err != nil {
		return nil, err
	}
	if e.gate, err = access.NewGate(access.GateConfig{Tenant: cfg.Tenant, Kinds: cfg.Content, Billing: deps.Billing, Logger: e.log}); err != nil {
		return nil, err
	}
	if err := e.newCatalog(ctx, deps, now); err != nil {
		return nil, err
	}
	if cfg.Media != nil {
		if e.media, err = e.newMedia(ctx, deps, now); err != nil {
			return nil, err
		}
		if e.avatars, err = e.newAvatars(deps); err != nil {
			return nil, err
		}
		if err := e.catalog.BindMedia(catalog.Media{Registry: e.media.reg, Reader: e.media.reader, Versions: e.media.versions}); err != nil {
			return nil, err
		}
	}
	if e.content, err = content.New(ctx, e.contentOptions(deps)); err != nil {
		return nil, err
	}
	if err := e.content.AddHeldSource(catalog.HeldKind, e.catalog); err != nil {
		return nil, err
	}
	e.catalog.BindModeration(e.content)
	e.notices = e.content.Notifications
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
	if err := cfg.Content.Validate(); err != nil {
		return fmt.Errorf("contentkit: Config.Content: %w", err)
	}
	if err := e.validateGroupScopes(deps.Scopes); err != nil {
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
		h := cfg.Media.Hooks
		if h.Resolver != nil || h.CanUpload != nil || h.PurgePublic != nil || h.ItemReady != nil || h.ItemCommitted != nil || h.Failed != nil {
			return errors.New("contentkit: Config.Media.Hooks is set: media's providers come from Deps")
		}
		if deps.Store == nil {
			return errors.New("contentkit: Config.Media needs Deps.Store")
		}
	}
	if a := cfg.Avatars; a != nil {
		if cfg.Media == nil {
			return errors.New("contentkit: Config.Avatars needs Config.Media")
		}
		if a.Primary != "" {
			if u, err := url.Parse(a.Primary); err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
				return fmt.Errorf("contentkit: Config.Avatars.Primary %q is not an http(s) URL", a.Primary)
			}
			if strings.TrimSpace(a.Key) == "" {
				return errors.New("contentkit: Config.Avatars.Primary needs Key")
			}
		}
	}
	if (deps.ClickHouse != nil || cfg.ClickHouse.Addr != "") && strings.TrimSpace(cfg.ClickHouse.Database) == "" {
		return errors.New("contentkit: the signal plane needs Config.ClickHouse.Database")
	}
	return nil
}

// migrate applies the catalog, account metadata, River and media worker
// migrations under their migrators' locks, then claims the configured scope.
func (e *Engine) migrate(ctx context.Context) error {
	return e.migratePostgres(ctx, MigrationOptions{})
}

func (e *Engine) migratePostgres(ctx context.Context, options MigrationOptions) error {
	db := stdlib.OpenDBFromPool(e.pool)
	defer db.Close()
	if err := migrations.ApplyPostgres(ctx, db, e.schema); err != nil {
		return err
	}
	if n := e.cfg.Notifications; n != nil {
		if !schemaRE.MatchString(n.Schema) || strings.TrimSpace(n.Namespace) == "" {
			return errors.New("contentkit: notifications require a valid schema and namespace")
		}
		if err := notification.Migrate(ctx, e.pool, n.Schema); err != nil {
			return err
		}
	}
	accountSchema, _ := e.accountStorage()
	if err := migrations.ApplyAccounts(ctx, db, accountSchema); err != nil {
		return err
	}
	if options.LegacyAccountKey != "" {
		if e.scope == nil || !layout.ValidSegment(options.LegacyAccountKey) {
			return errors.New("contentkit: LegacyAccountKey requires Scope and a valid key")
		}
		e.cfg.Notifications.Namespace = options.LegacyAccountKey
	}
	if err := e.resolveAccountKey(ctx); err != nil {
		return err
	}
	if options.LegacyAccountKey != "" && e.cfg.Notifications.Namespace != options.LegacyAccountKey {
		return ErrScopeConflict
	}
	if err := e.bindScope(ctx); err != nil {
		return err
	}
	if err := e.avatarPlacement(ctx, options.explicit); err != nil {
		return err
	}
	if options.SkipQueues {
		return nil
	}
	if err := riverhelpers.ApplyMigrations(ctx, e.pool, e.riverSchema); err != nil {
		return fmt.Errorf("contentkit: River migrations: %w", err)
	}
	if account, ok := e.accountMediaStorage(); ok {
		if err := workqueue.Migrate(ctx, e.pool, account.WorkerSchema); err != nil {
			return fmt.Errorf("contentkit: account media queue migrations: %w", err)
		}
	}
	if m := e.cfg.Media; m != nil {
		if m.WorkerSchema == "" {
			m.WorkerSchema = e.schema + "_media_worker"
		}
		if err := workqueue.Migrate(ctx, e.pool, m.WorkerSchema); err != nil {
			return fmt.Errorf("contentkit: media worker queue migrations: %w", err)
		}
	}
	return nil
}

// newCatalog builds the stores media is built over: groups, offers (with
// Deps.Billing) and the catalog, which refreshes facets on groups' changes
// and renames offers' products after titles.
func (e *Engine) newCatalog(ctx context.Context, deps Deps, now func() time.Time) error {
	c := e.cfg.Content
	opts := groups.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: c, Scopes: deps.Scopes, Logger: e.log, References: deps.GroupReferences,
		ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
			return e.catalog.ContentChanged(ctx, q, ids)
		},
		NamesChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
			return e.catalog.GroupsRenamed(ctx, q, ids)
		},
		DocumentsChanged: func(ctx context.Context, q groups.Querier) error { return e.syncDocuments(ctx, q) },
		Owned:            ownedContent{e}}
	if e.cfg.Media != nil {
		opts.Covers, opts.Folders = groupCovers{e}, groupFolders{e}
	}
	var err error
	if deps.Billing != nil {
		if e.offers, err = offers.New(offers.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: c, Config: e.cfg.Offers,
			Billing: deps.Billing, Gate: e.gate, Logger: e.log}); err != nil {
			return err
		}
		opts.Sales = e.offers
	}
	if e.groups, err = groups.New(opts); err != nil {
		return err
	}
	co := catalog.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: c, Auth: e.auth, Gate: e.gate, Groups: e.groups,
		Body: deps.BodyProcessor, Now: now, Logger: e.log,
		DocumentsChanged: func(ctx context.Context, q catalog.Querier) error { return e.syncDocuments(ctx, q) }}
	if e.offers != nil {
		co.Offers = e.offers
	}
	if e.catalog, err = catalog.New(ctx, co); err != nil {
		return err
	}
	e.catalog.Subscribe(e.forgetPurged)
	return nil
}

func (e *Engine) newMedia(ctx context.Context, deps Deps, now func() time.Time) (*engineMedia, error) {
	mc := e.cfg.Media
	reg, err := e.cfg.MediaKinds()
	if err != nil {
		return nil, err
	}
	m := &engineMedia{store: deps.Store}
	declared := map[string]bool{}
	for _, k := range reg.Kinds {
		declared[k.Name] = true
	}
	m.polls = declared[pollFolder]
	if m.versions, err = versions.New(versions.Options{Tenant: e.cfg.Tenant, Schema: e.schema, Pool: e.pool, Kinds: e.cfg.Content, Auth: e.auth, AccountExists: e.accountExists(deps),
		Resolver: e.catalog.Resolver(), Gate: e.gate, Facets: e.catalog, Others: siteMedia{e},
		Downloads: versions.Downloads{Anonymous: mc.Downloads.Anonymous, User: mc.Downloads.User, Tiers: mc.Downloads.Tiers, TTL: mc.Downloads.TTL, Redis: deps.Redis},
		Policy:    deps.Downloads, Logger: e.log, Now: now}); err != nil {
		return nil, err
	}
	reg.Hooks = m.versions.Hooks()
	reg.Hooks.PurgePublic = deps.PurgePublic
	if m.reg, err = media.NewRegistry(reg); err != nil {
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
	journal, err := media.NewPGJournal(e.pool, e.schema, m.queue)
	if err != nil {
		return nil, err
	}
	if sharedAccounts {
		accountJournal, err := media.NewPGJournal(e.pool, accountStorage.Schema, m.queue)
		if err != nil {
			return nil, err
		}
		journal, err = media.RouteJournal(journal, accountJournal, e.accountMediaPartition())
		if err != nil {
			return nil, err
		}
	}
	limiter, err := e.groupLimiter()
	if err != nil {
		return nil, err
	}
	if sharedAccounts && limiter != nil {
		accountLimiter, err := media.NewPGLimiter(e.pool, accountStorage.Schema, media.PGLimits{})
		if err != nil {
			return nil, err
		}
		limiter = media.RouteLimiter(limiter, accountLimiter, e.accountMediaPartition())
	}
	erasureQueue, err := media.NewHostQueue(e.pool, m.reg, e.riverSchema, "", 0)
	if err != nil {
		return nil, err
	}
	if m.jobs, err = media.NewJobs(media.JobsConfig{Store: deps.Store, Registry: m.reg, Locker: media.PGLocker(e.pool), Journal: journal, ErasureQueue: erasureQueue,
		Processes: m.queue, Pool: e.pool, Logger: e.log, Now: now, Limiter: limiter}); err != nil {
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
	var frames media.FrameGrabber
	if f, err := video.NewFrames(deps.Store); err == nil {
		frames = f
	} else {
		e.log.Warn("contentkit: no frame picker", "err", err) // it needs ffmpeg on PATH
	}
	rate := func(r RateLimitConfig) media.RateLimit {
		return media.RateLimit{PerSecond: r.PerSecond, Burst: r.Burst, Disabled: r.Disabled, Redis: deps.Redis}
	}
	if m.uploads, err = media.NewUploads(media.UploadOptions{Store: deps.Store, Manifests: m.jobs.Manifests(), Tickets: &ring, Frames: frames,
		ProcessOnUpload: mc.ProcessOnUpload, Commits: rate(mc.CommitLimit), Limiter: limiter}); err != nil {
		return nil, err
	}
	if m.progress, err = workqueue.NewProgressSourceWithAccounts(e.pool, m.reg, mc.WorkerSchema, accountQueue); err != nil {
		return nil, err
	}
	mode := media.DeliverURL
	if d.Mode != "" {
		mode = media.DeliveryMode(d.Mode)
	}
	if m.reader, err = media.NewReader(media.ReaderOptions{Manifests: m.jobs.Manifests(), Queue: m.queue, Progress: m.progress,
		Delivery: media.Delivery{Mode: mode, CookieDomain: d.CookieDomain, SigningKey: signing, TTL: d.TTL, Window: d.Window},
		Issuance: media.Issuance{PerHour: mc.Issuance.PerHour, AnonymousPerHour: mc.Issuance.AnonymousPerHour, Disabled: mc.Issuance.Disabled, Redis: deps.Redis},
		Now:      now}); err != nil {
		return nil, err
	}
	m.limit = rate(mc.ReadLimit)
	if err := m.versions.Bind(versions.Media{Manifests: m.jobs.Manifests(), Uploads: m.uploads, Jobs: m.jobs, Reader: m.reader}); err != nil {
		return nil, err
	}
	return m, nil
}

// groupLimiter limits uploads to user-owned groups by their kind's
// kinds.UploadLimits, keyed by the group (versions.GroupOwner); nil when no
// kind sets any.
func (e *Engine) groupLimiter() (*media.PGLimiter, error) {
	limits := map[string]kinds.UploadLimits{}
	for name, gk := range e.cfg.Content.GroupKinds {
		if gk.Uploads != (kinds.UploadLimits{}) {
			limits[name] = gk.Uploads
		}
	}
	if len(limits) == 0 {
		return nil, nil
	}
	of := func(owner string) kinds.UploadLimits {
		kind, _, _ := versions.ParseGroupOwner(owner)
		return limits[kind]
	}
	return media.NewPGLimiter(e.pool, e.schema, media.PGLimits{
		Quota: func(_ context.Context, _, owner string) (int64, error) { return of(owner).QuotaBytes, nil },
		Rates: func(_ context.Context, _, owner string) (int, int64, error) {
			l := of(owner)
			return l.FilesPerHour, l.BytesPerDay, nil
		}})
}

func (e *Engine) contentOptions(deps Deps) content.Options {
	in := e.cfg.Interactions
	rates := func(rs []Rate) []content.Rate {
		var out []content.Rate
		for _, r := range rs {
			out = append(out, content.Rate{Count: r.Count, Per: r.Per})
		}
		return out
	}
	l := in.Limits
	o := content.Options{
		Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Notifications: e.cfg.Notifications, Auth: e.auth, UserInfo: deps.Users, Resolver: interactionTargets{e}, Kinds: e.cfg.Content,
		Processor: deps.Processor,
		Moderator: deps.Moderator, ModeratorTimeout: in.ModeratorTimeout, ModeratorCooldown: in.ModeratorCooldown,
		Classifier: deps.Classifier, ProviderDataEraser: deps.ProviderDataEraser,
		Limits: content.Limits{Disabled: l.Disabled, Redis: deps.Redis, Comment: rates(l.Comment), CommentReaction: rates(l.CommentReaction),
			Reaction: rates(l.Reaction), Favorite: rates(l.Favorite), PollVote: rates(l.PollVote)},
		Anonymous: in.Anonymous, CommentMaxLength: in.CommentMaxLength,
		Logger: e.log,
	}
	if e.scope != nil {
		o.NotificationAppID = e.scope.AppID.String()
		o.AccountExists = e.accountExists(deps)
		o.AccountExistsTx = e.accountExistsTx(deps)
	}
	if e.media != nil {
		o.Media = &content.Media{Namespace: e.media.reg.Namespace(), Images: e.media.jobs.Manifests(), Folders: e.media.jobs, Reader: e.media.reader}
	}
	if e.avatars != nil {
		o.Avatars = e.avatars.urls
	}
	return o
}

type transactionalUserInfo interface {
	GetTx(context.Context, pgx.Tx, []string) (map[string]userinfo.User, error)
}

func (e *Engine) accountExistsTx(deps Deps) func(context.Context, pgx.Tx, string) (bool, error) {
	if e.scope == nil {
		return nil
	}
	return func(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
		directory, ok := deps.Users.(transactionalUserInfo)
		if !ok {
			return false, auth.ErrUnavailable
		}
		users, err := directory.GetTx(ctx, tx, []string{id})
		if err != nil {
			return false, err
		}
		user, found := users[id]
		return found && user.ID == id, nil
	}
}

// accountExists uses only the host's target-tenant directory. Source actor
// identity is not proof of target membership, including for global staff.
func (e *Engine) accountExists(deps Deps) func(context.Context, string) (bool, error) {
	if e.scope == nil {
		return nil
	}
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
	e.modules = map[httpapi.Module]http.Handler{httpapi.Content: e.content.Handler()}
	nh, err := e.notices.Handler(e.auth)
	if err != nil {
		return err
	}
	e.modules[httpapi.Notifications] = nh
	if m := e.media; m != nil {
		e.modules[httpapi.Upload] = media.UploadHandler(m.uploads, media.UploadHandlerOptions{Auth: e.auth, Logger: e.log})
		e.modules[httpapi.Media] = m.reader.Handler(media.HandlerOptions{Auth: e.auth, Logger: e.log, Limit: m.limit})
		if a := e.avatars; a != nil && a.forward != nil {
			e.modules[httpapi.Upload] = a.forward.wrap(httpapi.Upload, e.modules[httpapi.Upload])
			e.modules[httpapi.Media] = a.forward.wrap(httpapi.Media, e.modules[httpapi.Media])
		}
		if e.modules[httpapi.Versions], err = m.versions.Handler(e.auth, e.log); err != nil {
			return err
		}
		if e.publishing, err = publishing.New(publishing.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant,
			Catalog: e.catalog, Groups: e.groups, Registry: m.reg, Uploads: m.uploads, Manifests: m.jobs.Manifests(),
			Versions: m.versions, Auth: e.auth, Authority: publishingAuthority{e}, Logger: e.log, Clock: deps.Clock}); err != nil {
			return err
		}
		e.modules[httpapi.Publishing] = e.publishing.Handler()
	}
	c := e.cfg.Content
	if e.modules[httpapi.Entities], err = e.groups.Handler(e.auth); err != nil {
		return err
	}
	e.modules[httpapi.Contents] = e.catalog.Handler()
	if err := e.mountDiscovery(deps); err != nil {
		return err
	}
	var avatars *media.PresetRule
	if e.avatars != nil {
		avatars = e.avatars.rule
	}
	if e.modules[httpapi.Kinds], err = kindsroute.Handler(c, e.hub.SignalPlane, avatars); err != nil {
		return err
	}
	if e.importer, err = importer.New(importer.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Catalog: e.catalog, Groups: e.groups,
		Content: e.content, Offers: e.offers, Signals: e.discover, Rebuild: e.rebuild, Logger: e.log}); err != nil {
		return err
	}
	if e.modules[httpapi.Import], err = e.importer.Handler(e.auth); err != nil {
		return err
	}
	if e.offers != nil {
		if e.modules[httpapi.Offers], err = e.offers.Handler(e.auth); err != nil {
			return err
		}
	}
	urls, err := contenturl.New(contenturl.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant})
	if err != nil {
		return err
	}
	router, err := contenturl.NewRouter(urls, contenturl.RouterOptions{Routes: c.Routes(), Languages: c.Languages, Logger: e.log,
		Visibility: e.catalog.Visibility})
	if err != nil {
		return err
	}
	e.modules[httpapi.Codes] = router.Handler((&gate.Gate{Auth: e.auth, Log: e.log}).Guard)
	e.inproc = e.dispatch("", e.modules)
	return nil
}

// mountDiscovery builds the public read routes over the catalog's rule.
func (e *Engine) mountDiscovery(deps Deps) error {
	o := discovery.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: e.cfg.Content, Resolver: e.catalog.Resolver(), Gate: e.gate,
		Groups: e.groups, Hub: e.hub, Candidates: deps.Candidates, Logger: e.log, Now: deps.Clock, Visibility: e.catalog, ActivitySigner: deps.ActivitySigner,
		MeasuredEngagement: e.cfg.MeasuredEngagement}
	o.Tenure, _ = deps.Billing.(access.TenureReader)
	if o.Candidates != nil && !e.hub.SignalsConfigured() {
		e.log.Warn("contentkit: Deps.Candidates needs the signal plane; similar and recommendations are off")
		o.Candidates = nil
	}
	if e.media != nil {
		o.Media = e.media.versions
		if o.ActivitySigner == nil {
			o.ActivitySigner = e.media.reader
		}
	}
	if e.offers != nil {
		o.Sales = e.offers
	}
	var err error
	if e.discover, err = discovery.New(o); err != nil {
		return err
	}
	e.catalog.BindStanding(e.discover)
	e.modules[httpapi.Discovery], err = e.discover.Handler(e.auth)
	return err
}

// StartOption adjusts Start.
type StartOption func(*startOptions)

type startOptions struct {
	fleet    *river.Client[pgx.Tx]
	fleetSet bool
}

// WithRiverClient runs ContentKit's jobs on the host's River fleet, which
// riverhelpers.New built with RiverJobs in Config.Database.RiverSchema
// (beside the host's, AuthKit's and OpenRails' jobs). ContentKit enqueues
// through it and never starts or stops it.
func WithRiverClient(fleet *river.Client[pgx.Tx]) StartOption {
	return func(o *startOptions) { o.fleet, o.fleetSet = fleet, true }
}

// Start starts ContentKit's background work: search indexing, preference
// sync, projection repair, erasure and the media jobs. With no
// options it builds and runs ContentKit's own River client in
// Config.Database.RiverSchema; with WithRiverClient the jobs run on the host's
// fleet. It runs no DDL; ctx ending stops nothing: Close stops what Start
// started. Call it once, before serving.
func (e *Engine) Start(ctx context.Context, opts ...StartOption) error {
	if e.erasureRecovery {
		return ErrErasureReplayRequired
	}
	var o startOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed:
		return errors.New("contentkit: engine closed")
	case e.started:
		return errors.New("contentkit: Start called twice")
	case o.fleetSet && o.fleet == nil:
		return errors.New("contentkit: WithRiverClient requires a River client")
	case o.fleetSet:
		if !e.requested || e.jobs.bound() != o.fleet {
			return errors.New("contentkit: the fleet was not built with this engine's RiverJobs")
		}
		e.fleet, e.started = o.fleet, true
		return nil
	case e.requested:
		return errors.New("contentkit: RiverJobs went to a host fleet: pass it with WithRiverClient")
	}
	own, err := riverhelpers.New(ctx, e.pool, &river.Config{Schema: e.riverSchema, Queues: map[string]river.QueueConfig{}, Logger: e.log},
		e.jobs.contribution())
	if err != nil {
		return err
	}
	if err := own.Start(ctx); err != nil {
		return fmt.Errorf("contentkit: start River: %w", err)
	}
	e.own, e.started = own, true
	return nil
}

// Close stops what Start started (ContentKit's own River, never the host's
// fleet) and closes what New opened, leaving the host's pool, Redis and
// ClickHouse connections open.
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
	if e.signals != nil {
		e.signals.close()
	}
	var errs []error
	if e.own != nil {
		if err := e.own.Stop(ctx); err != nil {
			hard, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			errs = append(errs, err, e.own.StopAndCancel(hard))
			cancel()
		}
	}
	e.chMu.Lock()
	if e.ownCH != nil {
		errs = append(errs, e.ownCH.Close())
	}
	e.chMu.Unlock()
	return errors.Join(errs...)
}

// Ready reports whether the engine can serve: Postgres answers and its jobs
// run (the host's fleet bound to RiverJobs, or ContentKit's own River since
// Start). Optional dependencies never fail it (see Probes).
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
// the moderator (failing while its breaker holds writes for review).
func (e *Engine) Probes() []Probe { return slices.Clone(e.probes) }

func (e *Engine) newProbes(deps Deps) []Probe {
	var out []Probe
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

// EraseSubjects erases the shared account inbox/avatar plus this app's data.
// The identity lifecycle must invoke it for EVERY app of the tenant; AuthKit's
// AccountIssuers and durable OnPurge deliveries provide that fanout and retry.
// It does not discover or erase another engine's catalog on its own.
//
// It erases subjects from every plane: signals, interaction data,
// recent searches, inboxes, people's avatars (on the avatar primary only:
// another site's people's avatars are the primary's, which its own
// EraseSubjects erases; see AvatarsConfig) and data
// retained by moderation and classifier providers; approved authored content
// stays (content.EraseSubjects). Retry while error != nil or
// !report.Complete(); Remaining names a plane that could not finish
// (content_plane, media_plane, signal_plane). Without a signal plane only the
// content and media planes are erased; a configured one that is down stays
// signal_plane until it is back.
func (e *Engine) EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error) {
	return e.eraseSubjects(ctx, subjects, true)
}

// EraseAppSubjects permanently erases/fences personal activity in this app
// only. It preserves shared account notices, avatars, credentials and other
// apps' activity. It is not an account deregistration or an inbox reset API.
func (e *Engine) EraseAppSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error) {
	return e.eraseSubjects(ctx, subjects, false)
}

func (e *Engine) eraseSubjects(ctx context.Context, subjects []signal.Subject, account bool) (signal.ErasureReport, error) {
	invalid := signal.ErasureReport{Remaining: map[string]uint64{"invalid_subjects": 1}}
	if len(subjects) > signal.MaxErasureSubjects {
		return invalid, &signal.LimitError{Field: "subjects per erasure", Limit: signal.MaxErasureSubjects, Got: len(subjects)}
	}
	for _, s := range subjects {
		if err := s.Validate(); err != nil {
			return invalid, err
		}
	}
	report, signalErr := e.hub.EraseSubjects(ctx, subjects)
	if errors.Is(signalErr, hub.ErrSignalPlaneDown) {
		// It still holds their signals.
		_, cause := e.signals.state()
		signalErr = errors.Join(signalErr, cause)
	} else if errors.Is(signalErr, hub.ErrSignalPlaneDisabled) {
		signalErr = nil
	}
	if report.Remaining == nil {
		report.Remaining = map[string]uint64{}
	}
	var actors []string
	for _, s := range subjects {
		if s.Kind() == signal.SubjectKindUser {
			actors = append(actors, s.Key())
		}
	}
	eraseContent := e.content.EraseAppSubjects
	if account {
		eraseContent = e.content.EraseSubjects
	}
	contentErr := errors.Join(eraseContent(ctx, actors), e.discover.EraseSearches(ctx, subjects))
	var mediaErr error
	if account {
		mediaErr = e.eraseAvatars(ctx, actors)
	}
	if signalErr != nil {
		report.Remaining["signal_plane"] = 1
	}
	if contentErr != nil {
		report.Remaining["content_plane"] = 1
	}
	if mediaErr != nil {
		report.Remaining["media_plane"] = 1
	}
	err := errors.Join(signalErr, contentErr, mediaErr)
	if err == nil && report.Complete() && len(actors) > 0 {
		err = e.completeErasure(ctx, actors, account)
		if err != nil {
			report.Remaining["content_plane"] = 1
		}
	}
	return report, err
}

// eraseAvatars permanently fences and verifies each tenant account avatar.
// Completion means the first storage cleanup finished, not just job acceptance.
func (e *Engine) eraseAvatars(ctx context.Context, users []string) error {
	if e.scope != nil && len(users) > 0 {
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
		if err = e.media.jobs.Erase(ctx, media.Deletion{Ref: ref, Owner: u, OperationID: "account-erasure:" + u}); err != nil {
			return err
		}
	}
	return nil
}
