// Package contentkit is ContentKit running in this process: the content
// library a host configures and mounts, as it does OpenRails and AuthKit.
//
//	ck, err := contentkit.New(ctx, cfg, deps)  // migrates its schema
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
	"maps"
	"net/http"
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
	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/chmigrate"
	"github.com/riverqueue/river"

	"github.com/open-rails/authkit/auth"

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
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/contentkit/offers"
	"github.com/open-rails/contentkit/signal"
)

// DefaultSchema is Config.Database.Schema's default.
const DefaultSchema = "contentkit"

var schemaRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Engine is ContentKit running in this process for one tenant.
type Engine struct {
	cfg         Config
	schema      string
	riverSchema string
	pool        *pgxpool.Pool
	log         *slog.Logger
	auth        auth.Authenticator
	gate        *access.Gate
	classifier  bool // free-text answers to reclassify

	hub      *hub.Hub
	ch       signal.Conn
	ownCH    clickhouse.Conn
	content  *content.Runtime
	notices  *notification.Store
	groups   *groups.Store
	catalog  *catalog.Store
	offers   *offers.Service
	discover *discovery.Service
	importer *importer.Service
	media    *engineMedia
	modules  map[httpapi.Module]http.Handler
	inproc   http.Handler
	probes   []Probe

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

// New runs ContentKit in this process. Before anything else touches the
// database it creates or upgrades ContentKit's tables in
// Config.Database.Schema, River's in Config.Database.RiverSchema and the
// media worker's queue; replicas booting together take turns on an advisory
// lock. With ClickHouse.Addr it applies the signal plane's migrations too.
// It starts no jobs: Start does, and jobs queued before it wait. ctx bounds
// the wait for the databases.
func New(ctx context.Context, cfg Config, deps Deps) (_ *Engine, err error) {
	e := &Engine{cfg: cfg, pool: deps.Postgres, auth: deps.Auth, log: deps.Logger, classifier: deps.Classifier != nil}
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
	defer func() {
		if err != nil {
			_ = e.Close(context.WithoutCancel(ctx))
		}
	}()
	if err := e.migrate(ctx); err != nil {
		return nil, err
	}
	if err := e.openSignalPlane(ctx, deps); err != nil {
		return nil, err
	}
	if e.hub, err = hub.New(hub.Config{PG: e.pool, PGSchema: e.schema, Tenant: cfg.Tenant, DefaultLanguage: cfg.Content.Languages[0],
		CH: e.ch, CHDatabase: cfg.ClickHouse.Database}); err != nil {
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
		if err := e.catalog.BindMedia(catalog.Media{Images: e.media.jobs.Manifests(), Reader: e.media.reader, Versions: e.media.versions}); err != nil {
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
	return e, nil
}

func (e *Engine) validate(deps Deps) error {
	cfg := &e.cfg
	cfg.Tenant = strings.TrimSpace(cfg.Tenant)
	if cfg.Tenant == "" {
		return errors.New("contentkit: Config.Tenant is required")
	}
	e.schema = strings.TrimSpace(cfg.Database.Schema)
	if e.schema == "" {
		e.schema = DefaultSchema
	}
	e.riverSchema = strings.TrimSpace(cfg.Database.RiverSchema)
	if e.riverSchema == "" {
		e.riverSchema = e.schema + "_river"
	}
	for _, s := range []string{e.schema, e.riverSchema} {
		if !schemaRE.MatchString(s) || len(s) > 63 {
			return fmt.Errorf("contentkit: schema %q is not a lower-case identifier", s)
		}
	}
	if err := cfg.Content.Validate(); err != nil {
		return fmt.Errorf("contentkit: Config.Content: %w", err)
	}
	for kind := range cfg.Content.Kinds {
		if slices.Contains(httpapi.Reserved(), kind) {
			return fmt.Errorf("contentkit: content kind %q is a route group's path", kind)
		}
	}
	if deps.Postgres == nil {
		return errors.New("contentkit: Deps.Postgres is required")
	}
	if err := gate.Check(deps.Auth, access.Permissions()...); err != nil {
		return fmt.Errorf("contentkit: Deps.Auth: %w", err)
	}
	if cfg.Media != nil {
		mc := *cfg.Media
		cfg.Media = &mc
		switch mc.Namespace {
		case "":
			mc.Namespace = cfg.Tenant
		case cfg.Tenant:
		default:
			return fmt.Errorf("contentkit: Config.Media.Namespace %q is not the tenant %q", mc.Namespace, cfg.Tenant)
		}
		h := cfg.Media.Hooks
		if h.Resolver != nil || h.CanUpload != nil || h.PurgePublic != nil || h.ItemReady != nil || h.ItemCommitted != nil || h.Failed != nil {
			return errors.New("contentkit: Config.Media.Hooks is set: media's providers come from Deps")
		}
		if deps.Store == nil {
			return errors.New("contentkit: Config.Media needs Deps.Store")
		}
	}
	if (deps.ClickHouse != nil || cfg.ClickHouse.Addr != "") && strings.TrimSpace(cfg.ClickHouse.Database) == "" {
		return errors.New("contentkit: the signal plane needs Config.ClickHouse.Database")
	}
	return nil
}

// migrate applies ContentKit's, River's and the media worker queue's
// migrations, each under its migrator's advisory lock.
func (e *Engine) migrate(ctx context.Context) error {
	db := stdlib.OpenDBFromPool(e.pool)
	defer db.Close()
	if err := migrations.ApplyPostgres(ctx, db, e.schema); err != nil {
		return err
	}
	if err := riverhelpers.ApplyMigrations(ctx, e.pool, e.riverSchema); err != nil {
		return fmt.Errorf("contentkit: River migrations: %w", err)
	}
	if m := e.cfg.Media; m != nil {
		if m.WorkerSchema == "" {
			m.WorkerSchema = e.schema + "_media_worker"
		}
		if err := workqueue.Migrate(ctx, e.pool, m.WorkerSchema); err != nil {
			return fmt.Errorf("contentkit: media worker queue migrations: %w", err)
		}
	}
	c := e.cfg.ClickHouse
	if c.Addr == "" {
		return nil
	}
	steps, err := migratekit.Load(migrations.ClickHouse, ".")
	if err != nil {
		return fmt.Errorf("contentkit: load signal migrations: %w", err)
	}
	m := chmigrate.New(&chmigrate.Config{ClientAddr: c.Addr, Database: c.Database, Username: c.Username, Password: c.Password,
		Cluster: c.Cluster, App: "contentkit_signal", PostgresDB: db})
	defer m.Close()
	if err := m.ApplyMigrations(ctx, steps); err != nil {
		return fmt.Errorf("contentkit: apply signal migrations: %w", err)
	}
	return nil
}

// openSignalPlane takes Deps.ClickHouse or opens one from Config, and refuses
// a database whose schema this version does not use.
func (e *Engine) openSignalPlane(ctx context.Context, deps Deps) error {
	c := e.cfg.ClickHouse
	e.ch = deps.ClickHouse
	if e.ch == nil && c.Addr != "" {
		conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{c.Addr},
			Auth: clickhouse.Auth{Database: c.Database, Username: c.Username, Password: c.Password}})
		if err != nil {
			return fmt.Errorf("contentkit: open ClickHouse: %w", err)
		}
		e.ch, e.ownCH = conn, conn
	}
	if e.ch == nil {
		return nil
	}
	return signal.CheckSchema(ctx, e.ch, c.Database)
}

// newCatalog builds the stores media is built over: groups, offers (with
// Deps.Billing) and the catalog, which refreshes facets on groups' changes
// and renames offers' products after titles.
func (e *Engine) newCatalog(ctx context.Context, deps Deps, now func() time.Time) error {
	c := e.cfg.Content
	opts := groups.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: c, Logger: e.log,
		ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
			return e.catalog.ContentChanged(ctx, q, ids)
		}}
	if e.cfg.Media != nil {
		opts.Covers = groupCovers{e}
	}
	var err error
	if e.groups, err = groups.New(opts); err != nil {
		return err
	}
	co := catalog.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: c, Auth: e.auth, Gate: e.gate, Groups: e.groups,
		Body: deps.BodyProcessor, Now: now, Logger: e.log}
	if deps.Billing != nil {
		if e.offers, err = offers.New(offers.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: c, Config: e.cfg.Offers,
			Billing: deps.Billing, Gate: e.gate, Logger: e.log}); err != nil {
			return err
		}
		co.Offers = e.offers
	}
	e.catalog, err = catalog.New(ctx, co)
	return err
}

func (e *Engine) newMedia(ctx context.Context, deps Deps, now func() time.Time) (*engineMedia, error) {
	mc := e.cfg.Media
	reg, err := media.ForContent(mc.Config, e.cfg.Content)
	if err != nil {
		return nil, err
	}
	m := &engineMedia{store: deps.Store}
	declared := map[string]bool{}
	for _, k := range reg.Kinds {
		declared[k.Name] = true
	}
	m.polls = declared[pollFolder]
	if m.versions, err = versions.New(versions.Options{Tenant: e.cfg.Tenant, Schema: e.schema, Pool: e.pool, Kinds: e.cfg.Content, Auth: e.auth,
		Resolver: e.catalog.Resolver(), Gate: e.gate, Facets: e.catalog, Others: pollMedia{e},
		Downloads: versions.Downloads{Anonymous: mc.Downloads.Anonymous, User: mc.Downloads.User, Tiers: mc.Downloads.Tiers, Redis: deps.Redis},
		Logger:    e.log, Now: now}); err != nil {
		return nil, err
	}
	reg.Hooks = m.versions.Hooks()
	reg.Hooks.PurgePublic = deps.PurgePublic
	if m.reg, err = media.NewRegistry(reg); err != nil {
		return nil, err
	}
	if m.queue, err = workqueue.New(e.pool, m.reg, mc.WorkerSchema); err != nil {
		return nil, err
	}
	journal, err := media.NewPGJournal(e.pool, e.schema, m.queue)
	if err != nil {
		return nil, err
	}
	if m.jobs, err = media.NewJobs(media.JobsConfig{Store: deps.Store, Registry: m.reg, Locker: media.PGLocker(e.pool), Journal: journal,
		Processes: m.queue, Pool: e.pool, Logger: e.log, Now: now}); err != nil {
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
		ProcessOnUpload: mc.ProcessOnUpload, Commits: rate(mc.CommitLimit)}); err != nil {
		return nil, err
	}
	if m.progress, err = workqueue.NewProgressSource(e.pool, mc.WorkerSchema); err != nil {
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
	if err := m.versions.Bind(versions.Media{Manifests: m.jobs.Manifests(), Uploads: m.uploads, Jobs: m.jobs}); err != nil {
		return nil, err
	}
	return m, nil
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
		Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Auth: e.auth, UserInfo: deps.Users, Resolver: e.catalog.Resolver(),
		Processor: deps.Processor,
		Moderator: deps.Moderator, ModeratorTimeout: in.ModeratorTimeout, ModeratorCooldown: in.ModeratorCooldown,
		Classifier: deps.Classifier, ProviderDataEraser: deps.ProviderDataEraser,
		Limits: content.Limits{Disabled: l.Disabled, Redis: deps.Redis, Comment: rates(l.Comment), CommentReaction: rates(l.CommentReaction),
			Reaction: rates(l.Reaction), Favorite: rates(l.Favorite), PollVote: rates(l.PollVote)},
		Anonymous: in.Anonymous, CommentMaxLength: in.CommentMaxLength,
		ContentKinds: slices.Sorted(maps.Keys(e.cfg.Content.Kinds)),
		Logger:       e.log,
	}
	if e.media != nil {
		o.Media = &content.Media{Images: e.media.jobs.Manifests(), Folders: e.media.jobs, Reader: e.media.reader}
	}
	return o
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
		if e.modules[httpapi.Versions], err = m.versions.Handler(e.auth, e.log); err != nil {
			return err
		}
	}
	c := e.cfg.Content
	if e.modules[httpapi.Groups], err = e.groups.Handler(e.auth); err != nil {
		return err
	}
	e.modules[httpapi.Contents] = e.catalog.Handler()
	if e.modules[httpapi.Kinds], err = kindsroute.Handler(c); err != nil {
		return err
	}
	if err := e.mountDiscovery(deps); err != nil {
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
	e.modules[httpapi.Codes] = router.Handler()
	e.inproc = e.dispatch("", e.modules)
	return nil
}

// mountDiscovery builds the public read routes over the catalog's rule.
func (e *Engine) mountDiscovery(deps Deps) error {
	o := discovery.Options{Pool: e.pool, Schema: e.schema, Tenant: e.cfg.Tenant, Kinds: e.cfg.Content, Resolver: e.catalog.Resolver(), Gate: e.gate,
		Groups: e.groups, Hub: e.hub, Candidates: deps.Candidates, Logger: e.log, Now: deps.Clock, Visibility: e.catalog}
	if e.media != nil {
		o.Covers = e.media.versions
	}
	var err error
	if e.discover, err = discovery.New(o); err != nil {
		return err
	}
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
// sync, co-engagement, projection repair, erasure and the media jobs. With no
// options it builds and runs ContentKit's own River client in
// Config.Database.RiverSchema; with WithRiverClient the jobs run on the host's
// fleet. It runs no DDL; ctx ending stops nothing: Close stops what Start
// started. Call it once, before serving.
func (e *Engine) Start(ctx context.Context, opts ...StartOption) error {
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
	var errs []error
	if e.own != nil {
		if err := e.own.Stop(ctx); err != nil {
			hard, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			errs = append(errs, err, e.own.StopAndCancel(hard))
			cancel()
		}
	}
	if e.ownCH != nil {
		errs = append(errs, e.ownCH.Close())
	}
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
	if e.ch != nil {
		out = append(out, Probe{Name: "contentkit_clickhouse", Check: func(ctx context.Context) error {
			return e.ch.Exec(ctx, "SELECT 1")
		}})
	}
	if e.media != nil {
		store := e.media.store
		out = append(out, Probe{Name: "contentkit_media_store", Check: func(ctx context.Context) error {
			return store.Check(ctx, "_contentkit/")
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

// EraseSubjects erases subjects from every plane: signals, interaction data,
// inboxes and data retained by moderation and classifier providers; approved
// authored content stays (content.EraseSubjects). Retry while error != nil or
// !report.Complete(); Remaining names a plane that could not finish
// (content_plane, signal_plane). Without a signal plane only the content
// plane is erased.
func (e *Engine) EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error) {
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
	if errors.Is(signalErr, hub.ErrSignalPlaneDisabled) {
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
	contentErr := e.content.EraseSubjects(ctx, actors)
	if signalErr != nil {
		report.Remaining["signal_plane"] = 1
	}
	if contentErr != nil {
		report.Remaining["content_plane"] = 1
	}
	return report, errors.Join(signalErr, contentErr)
}
