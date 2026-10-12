// Package server is ContentKit as a standalone deployment: the engine behind
// its own HTTP server, an AuthKit resource server deciding who each request
// is, the trusted issuer's directory naming authors, and OpenRails' remote
// client answering entitlements.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/helpers/deps"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// StaffRole is the root role holding ContentKit's permissions: a trusted
// issuer's tokens reach staff routes through it (RemoteApplicationConfig.Role
// and RoleMap name it "root:staff").
const StaffRole = "staff"

// Config is a standalone deployment: plain data. Secrets are read from the
// environment into it (cmd/contentkit).
type Config struct {
	// Engine is what contentkit.New takes.
	Engine contentkit.Config `yaml:"engine"`
	// Auth is the server's AuthKit as a resource server (AuthKit's own
	// configuration): Token.Issuer, Resource.ID and the site's trusted
	// issuer in RemoteApplications. It mints nothing unless Keys says so;
	// its tables live in Database.Schema (default "contentkit_auth").
	Auth authkit.Config `yaml:"auth"`
	// UsersIssuer is the trusted issuer whose directory names authors; empty
	// is the only one RemoteApplications declares.
	UsersIssuer string `yaml:"users_issuer"`
	// OpenRails is where entitlements are read; an empty BaseURL runs
	// without billing.
	OpenRails OpenRailsConfig `yaml:"openrails"`
	// S3 is the media bucket, required with Engine.Media.
	S3 S3Config `yaml:"s3"`
	// Routes are the route groups served (contentkit.ParseRouteGroups names);
	// empty is every group the engine can serve.
	Routes []string `yaml:"routes"`
	// Prefix is where the API is served: "/api/contentkit" serves
	// /api/contentkit/v1/*. Empty is the root.
	Prefix string `yaml:"prefix"`
	// Addr is where Run listens; empty is ":8080".
	Addr string `yaml:"addr"`
	// DrainDelay is how long Run keeps serving once ctx ends, with /readyz
	// failing, so load balancers stop routing here first.
	DrainDelay time.Duration `yaml:"drain_delay"`
	// ShutdownTimeout bounds finishing in-flight requests; 0 is 30s.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// OpenRailsConfig is the OpenRails server holding the site's merchant.
type OpenRailsConfig struct {
	BaseURL string `yaml:"base_url"`
	// APIKey is the merchant's API key (a secret).
	APIKey string `yaml:"api_key"`
}

// S3Config is the media bucket.
type S3Config struct {
	Bucket string `yaml:"bucket"`
	Region string `yaml:"region"`
	// Endpoint is where the server reaches the bucket; PublicEndpoint where
	// browsers PUT presigned uploads (default Endpoint).
	Endpoint       string `yaml:"endpoint"`
	PublicEndpoint string `yaml:"public_endpoint"`
	UsePathStyle   bool   `yaml:"use_path_style"`
	// AccessKeyID and SecretAccessKey are secrets.
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
}

// Deps is what the server reaches outside its process.
type Deps struct {
	// Postgres holds ContentKit's, River's and AuthKit's schemas. Required.
	Postgres *pgxpool.Pool
	// Redis is optional: shared rate limits and DPoP replay state.
	Redis  redis.UniversalClient
	Logger *slog.Logger
}

// Server is a standalone ContentKit.
type Server struct {
	started bool
	cfg     Config
	log     *slog.Logger
	engine  *contentkit.Engine
	auth    *authkit.Client
	fleet   *river.Client[pgx.Tx]
	sup     *deps.Supervisor
	handler http.Handler
}

// New builds the server: AuthKit (which creates or upgrades its tables),
// then the engine (its own), one River fleet in the engine's River schema
// running both, and the HTTP surface. ctx bounds the wait for the database.
func New(ctx context.Context, cfg Config, d Deps) (_ *Server, err error) {
	if d.Postgres == nil {
		return nil, errors.New("server: Deps.Postgres is required")
	}
	s := &Server{cfg: cfg, log: d.Logger}
	if s.log == nil {
		s.log = slog.Default()
	}
	defer func() {
		if err != nil {
			_ = s.Close(context.WithoutCancel(ctx))
		}
	}()
	var groups contentkit.RouteGroups
	if len(cfg.Routes) > 0 {
		if groups, err = contentkit.ParseRouteGroups(cfg.Routes); err != nil {
			return nil, err
		}
	}
	riverSchema := cfg.Engine.Database.RiverSchema
	if riverSchema == "" {
		schema := cfg.Engine.Database.Schema
		if schema == "" {
			schema = contentkit.DefaultSchema
		}
		riverSchema = schema + "_river"
	}
	cfg.Engine.Database.RiverSchema = riverSchema

	ak := cfg.Auth
	if ak.Database.Schema == "" {
		ak.Database.Schema = "contentkit_auth"
	}
	ak.Database.RiverSchema = riverSchema
	if !ak.Keys.AllowEphemeralDevKeys && ak.Keys.Path == "" {
		ak.Keys.VerifyOnly = true // a resource server verifies; it mints nothing
	}
	rbac := authkit.NewRoles(authkit.RemoteApplications)
	var grants []iam.Grant
	for _, p := range rbac.Root.Declare(access.Names(access.Permissions()...)...) {
		grants = append(grants, p)
	}
	rbac.Root.Role(StaffRole, grants...)
	ak.Roles = rbac
	if s.auth, err = authkit.New(ctx, ak, authkit.Deps{Postgres: d.Postgres, Redis: d.Redis}); err != nil {
		return nil, fmt.Errorf("server: AuthKit: %w", err)
	}

	engineDeps := contentkit.Deps{Postgres: d.Postgres, Redis: d.Redis, Auth: s.auth.Authenticator(), Logger: s.log}
	if engineDeps.Users, err = s.users(ak); err != nil {
		return nil, err
	}
	if cfg.OpenRails.BaseURL != "" {
		billing, err := openrails.NewRemoteMerchant(cfg.OpenRails.BaseURL, openrails.RemoteCredential{APIKey: cfg.OpenRails.APIKey})
		if err != nil {
			return nil, fmt.Errorf("server: OpenRails: %w", err)
		}
		engineDeps.Billing = ckopenrails.Entitlements(billing)
	}
	if cfg.Engine.Media != nil {
		b := cfg.S3
		if engineDeps.Store, err = mediaS3.New(mediaS3.Config{Bucket: b.Bucket, Region: b.Region, Endpoint: b.Endpoint, PublicEndpoint: b.PublicEndpoint,
			UsePathStyle: b.UsePathStyle, AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey}); err != nil {
			return nil, fmt.Errorf("server: S3: %w", err)
		}
	}
	if s.engine, err = contentkit.New(ctx, cfg.Engine, engineDeps); err != nil {
		return nil, err
	}

	if s.fleet, err = riverhelpers.New(ctx, d.Postgres, &river.Config{Schema: riverSchema, Queues: map[string]river.QueueConfig{}, Logger: s.log},
		s.auth.RiverJobs(), s.engine.RiverJobs()); err != nil {
		return nil, err
	}
	if groups == 0 {
		groups = s.engine.RouteGroups()
	}
	routes, err := s.engine.Routes(contentkit.Routes{Prefix: cfg.Prefix, Groups: groups})
	if err != nil {
		return nil, err
	}
	s.sup = deps.New(deps.WithLogger(s.log))
	s.sup.AddPostgres("postgres", d.Postgres.Config().ConnConfig)
	for _, p := range s.engine.Probes() {
		s.sup.Add(p.Name, deps.Optional, deps.Probe(p.Check), nil, deps.ProbeTimeout(5*time.Second))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+deps.LivePath, s.sup.Livez)
	mux.HandleFunc("GET "+deps.ReadyPath, s.sup.Readyz)
	mux.HandleFunc("GET "+deps.StatusPath, s.sup.Statusz)
	for _, r := range routes {
		mux.Handle(strings.TrimSpace(r.Method+" "+r.Path), r.Handler)
	}
	s.handler = mux
	return s, nil
}

// users is the trusted issuer's directory in the root group.
func (s *Server) users(ak authkit.Config) (userinfo.Lookup, error) {
	issuer := s.cfg.UsersIssuer
	if issuer == "" {
		if len(ak.RemoteApplications) != 1 {
			return nil, nil
		}
		issuer = ak.RemoteApplications[0].Issuer
	}
	if !slices.ContainsFunc(ak.RemoteApplications, func(a authkit.RemoteApplicationConfig) bool { return a.Issuer == issuer }) {
		return nil, fmt.Errorf("server: users_issuer %q is not a declared remote application", issuer)
	}
	return s.auth.RemoteUserInfo(iam.RootGroup(), issuer), nil
}

// Handler is the public surface: ContentKit's routes and /livez, /readyz,
// /statusz.
func (s *Server) Handler() http.Handler { return s.handler }

// Start runs the jobs (AuthKit's and ContentKit's on one fleet) and marks
// the server ready once the engine is.
func (s *Server) Start(ctx context.Context) error {
	if err := s.fleet.Start(ctx); err != nil {
		return err
	}
	s.started = true
	if err := s.auth.Start(ctx, authkit.WithRiverClient(s.fleet)); err != nil {
		return err
	}
	if err := s.engine.Start(ctx, contentkit.WithRiverClient(s.fleet)); err != nil {
		return err
	}
	s.sup.Start(ctx)
	if err := s.engine.Ready(ctx); err != nil {
		return err
	}
	s.sup.SetReady()
	return nil
}

// Run starts the server and serves Config.Addr until ctx ends, then drains:
// /readyz fails for DrainDelay, in-flight requests finish within
// ShutdownTimeout, and the jobs stop.
func (s *Server) Run(ctx context.Context) error {
	addr := s.cfg.Addr
	if addr == "" {
		addr = ":8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on a listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.Start(ctx); err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	var serveErr error
	select {
	case serveErr = <-served:
	case <-ctx.Done():
		s.sup.Drain()
		if d := s.cfg.DrainDelay; d > 0 {
			time.Sleep(d)
		}
	}
	timeout := s.cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if serveErr == nil {
		serveErr = srv.Shutdown(shutdown)
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, s.Close(shutdown))
}

// Close stops the jobs and closes the engine and AuthKit, leaving the pool
// and Redis open.
func (s *Server) Close(ctx context.Context) error {
	var errs []error
	if s.started {
		if err := s.fleet.Stop(ctx); err != nil {
			errs = append(errs, err, s.fleet.StopAndCancel(context.WithoutCancel(ctx)))
		}
	}
	if s.engine != nil {
		errs = append(errs, s.engine.Close(ctx))
	}
	if s.auth != nil {
		errs = append(errs, s.auth.Close(ctx))
	}
	return errors.Join(errs...)
}
