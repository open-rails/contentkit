// Package server is ContentKit as a standalone deployment: the engine behind
// its own HTTP server, an AuthKit resource server deciding who each request
// is, the trusted issuer's directory naming authors, and OpenRails' remote
// client answering entitlements.
package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/helpers/deps"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/redis/go-redis/v9"

	contentkit "github.com/open-rails/contentkit"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// StaffRole names the declared ContentKit staff role: app:staff with native
// inherited authority.
const StaffRole = "staff"

// Config is a standalone deployment: plain data. Secrets are read from the
// environment into it (cmd/contentkit).
type Config struct {
	// Engine is what contentkit.New takes.
	Engine contentkit.Config `yaml:"engine"`
	// Auth is the server's AuthKit as a resource server (AuthKit's own
	// configuration): Issuer and Resource.ID. Its tables live in
	// Database.Schema (default "contentkit_auth"). It runs multi-tenant,
	// acting in the scope's tenant.
	Auth authkit.Config `yaml:"auth"`
	// AuthKeys is the directory of AuthKit's keys.json (and totp.key),
	// watched for rotation: the keys of Auth.Issuer's tokens. Empty holds
	// none: the server mints nothing and verifies only trusted issuers'
	// tokens and API keys.
	AuthKeys string `yaml:"auth_keys"`
	// Authorization binds the engine to explicitly provisioned AuthKit groups
	// and a verified tenant identity directory. Required.
	Authorization *AuthorizationConfig `yaml:"authorization"`
	// UsersIssuer is the trusted issuer whose directory names authors; the
	// engine uses Authorization and refuses a conflicting UsersIssuer.
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
	// ReadTimeout bounds reading a request's body, from when its headers
	// are read; 0 is 60s. Once the body is read nothing bounds the answer,
	// so event streams and archive downloads run as long as they need.
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// IdleTimeout bounds a kept-alive connection between requests; 0 is 120s.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

// maxHeaderBytes caps a request's header block.
const maxHeaderBytes = 64 << 10

// OpenRailsConfig is the OpenRails server holding the site's merchant.
type OpenRailsConfig struct {
	BaseURL string `yaml:"base_url"`
	// MerchantID is the site's merchant, the one APIKey is bound to.
	MerchantID billing.MerchantID `yaml:"merchant_id"`
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
	// Redis is optional: shared rate limits and DPoP replay state. While
	// it fails, AuthKit refuses DPoP proofs rather than spend them locally.
	Redis  redis.UniversalClient
	Logger *slog.Logger
}

// Server is a standalone ContentKit.
type Server struct {
	cfg     Config
	log     *slog.Logger
	engine  *contentkit.Engine
	auth    *authkit.Client
	keys    *keys.FileSource
	billing *openrails.Remote
	sup     *deps.Supervisor
	handler http.Handler
}

// New builds the server: AuthKit (which creates or upgrades its tables and
// its River schema), then the engine (its own, River's included), and the
// HTTP surface. ctx bounds the wait for the database.
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
	ak := cfg.Auth
	// The scope's tenant is one provisioned tenant of AuthKit's directory,
	// which root operators share with other tenants.
	ak.Tenancy = authkit.TenancyMulti
	if cfg.Engine.Scope != nil && ak.DefaultTenantID.IsZero() {
		ak.DefaultTenantID, err = iam.ParseTenantID(cfg.Engine.Scope.TenantID.String())
		if err != nil {
			return nil, err
		}
	}
	if ak.Database.Schema == "" {
		ak.Database.Schema = "contentkit_auth"
	}
	if ak.Database.RiverSchema == "" {
		ak.Database.RiverSchema = ak.Database.Schema + "_river"
	}
	if err := validateAuthorizationConfig(cfg, ak); err != nil {
		return nil, err
	}
	if err := distinctSchemas(cfg.Engine, ak.Database); err != nil {
		return nil, err
	}
	if ak.HTTP != nil {
		h := *ak.HTTP
		if err := authClientIP(&h, cfg.Engine.TrustedProxies); err != nil {
			return nil, err
		}
		ak.HTTP = &h
	}
	ak.Roles = serverRoles(cfg.Engine)
	var keySource keys.Source = keys.Static{} // no signer: it verifies, minting nothing
	if cfg.AuthKeys != "" {
		if s.keys, err = keys.Watch(cfg.AuthKeys, s.log); err != nil {
			return nil, fmt.Errorf("server: auth_keys: %w", err)
		}
		keySource = s.keys
	}
	if s.auth, err = authkit.New(ctx, ak, authkit.Deps{Postgres: d.Postgres, Redis: d.Redis, KeySource: keySource, Logger: s.log}); err != nil {
		return nil, fmt.Errorf("server: AuthKit: %w", err)
	}

	engineDeps := contentkit.Deps{Postgres: d.Postgres, Redis: d.Redis, Auth: s.auth.Authenticator(), Logger: s.log}
	if err := s.scopedAuthorization(ctx, cfg, ak, &engineDeps); err != nil {
		return nil, err
	}
	if cfg.OpenRails.BaseURL != "" {
		if err := secureEndpoint(cfg.OpenRails.BaseURL); err != nil {
			return nil, fmt.Errorf("server: openrails.base_url: %w", err)
		}
		id := cfg.OpenRails.MerchantID
		if id.IsZero() {
			return nil, errors.New("server: openrails.merchant_id is required with openrails.base_url")
		}
		if s.billing, err = openrails.NewRemote(cfg.OpenRails.BaseURL, openrails.RemoteOptions{
			Credentials: map[billing.MerchantID]openrails.RemoteCredential{id: {APIKey: cfg.OpenRails.APIKey}}, Logger: s.log,
		}); err != nil {
			return nil, fmt.Errorf("server: OpenRails: %w", err)
		}
		merchant, err := s.billing.Merchant(id)
		if err != nil {
			return nil, fmt.Errorf("server: OpenRails: %w", err)
		}
		engineDeps.Billing = ckopenrails.Billing(merchant)
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
	s.handler = boundReads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	}), cmp.Or(cfg.ReadTimeout, 60*time.Second))
	return s, nil
}

// boundReads bounds reading a request's body to d from when its headers are
// read. Only a body read to its end lifts the deadline, so the answer after
// it runs unbounded; a body that stalls or fails keeps it, which also bounds
// net/http's discard of what is left before it answers. It must wrap
// net/http's own ResponseWriter, and it hands the handlers a copy of the
// request, so net/http still sees its own body.
func boundReads(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			rc := http.NewResponseController(w)
			if err := rc.SetReadDeadline(time.Now().Add(d)); err == nil {
				r = r.WithContext(r.Context())
				r.Body = &boundedBody{ReadCloser: r.Body, lift: func() { _ = rc.SetReadDeadline(time.Time{}) }}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// boundedBody lifts the read deadline once the body is read to its end.
type boundedBody struct {
	io.ReadCloser
	once sync.Once
	lift func()
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.lift)
	}
	return n, err
}

// distinctSchemas refuses an AuthKit schema that is one of the engine's: they
// would share tables or a River leader. AuthKit may keep its tables and its
// River in one schema; that is AuthKit's to allow.
func distinctSchemas(engine contentkit.Config, auth authkit.DatabaseConfig) error {
	s, err := engine.Schemas()
	if err != nil {
		return err
	}
	for _, a := range []struct{ name, schema string }{{"auth.database.schema", auth.Schema}, {"auth.database.river_schema", auth.RiverSchema}} {
		for _, e := range []struct{ name, schema string }{{"engine.database.schema", s.Catalog}, {"engine.database.river_schema", s.River},
			{"engine.accounts.schema", s.Accounts}, {"engine.media.worker_schema", s.MediaWorker}, {"the account media queue", s.AccountWorker}} {
			if e.schema == a.schema {
				return fmt.Errorf("server: %s and %s are both %q", a.name, e.name, a.schema)
			}
		}
	}
	return nil
}

// authClientIP gives AuthKit the engine's client-address posture, its one
// source: engine.trusted_proxies, or the direct peer when there are none.
// auth.http may only restate those proxies (split out Cloudflare's, whose
// CF-Connecting-IP AuthKit reads); direct_peer_ip beside a proxy is refused.
func authClientIP(h *authkit.HTTPConfig, trusted []netip.Prefix) error {
	own := append(slices.Clone(h.TrustedProxies), h.CloudflareProxies...)
	switch {
	case h.DirectPeerIP && (len(trusted) > 0 || len(own) > 0):
		return errors.New("server: auth.http.direct_peer_ip says no proxy is in front, but a proxy is declared; declare the proxies only in engine.trusted_proxies")
	case len(own) == 0:
		h.TrustedProxies = make([]string, len(trusted))
		for i, p := range trusted {
			h.TrustedProxies[i] = p.String()
		}
		h.DirectPeerIP = len(trusted) == 0
	case !sameProxies(own, trusted):
		return errors.New("server: auth.http's proxies differ from engine.trusted_proxies; declare them once, in engine.trusted_proxies")
	}
	return nil
}

func sameProxies(own []string, trusted []netip.Prefix) bool {
	a := make([]netip.Prefix, 0, len(own))
	for _, s := range own {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return false
		}
		a = append(a, p.Masked())
	}
	b := make([]netip.Prefix, 0, len(trusted))
	for _, p := range trusted {
		b = append(b, p.Masked())
	}
	order := func(x, y netip.Prefix) int {
		return cmp.Or(x.Addr().Compare(y.Addr()), cmp.Compare(x.Bits(), y.Bits()))
	}
	slices.SortFunc(a, order)
	slices.SortFunc(b, order)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// secureEndpoint refuses a plain http URL off loopback: the API key would
// cross in clear.
func secureEndpoint(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return fmt.Errorf("%q is not an absolute URL", raw)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http":
		host := u.Hostname()
		if addr, err := netip.ParseAddr(host); err == nil && addr.Unmap().IsLoopback() || strings.EqualFold(host, "localhost") {
			return nil
		}
		return fmt.Errorf("%q is plain http; use https (http only for loopback)", raw)
	}
	return fmt.Errorf("%q is not an http(s) URL", raw)
}

// Reconcile repairs the engine's derived catalog state (Engine.Reconcile).
func (s *Server) Reconcile(ctx context.Context) (contentkit.Reconciled, error) {
	return s.engine.Reconcile(ctx)
}

// Handler is the public surface: ContentKit's routes and /livez, /readyz,
// /statusz.
func (s *Server) Handler() http.Handler { return s.handler }

// Start runs the jobs (AuthKit's and ContentKit's, each on its own River) and
// marks the server ready once the engine is.
func (s *Server) Start(ctx context.Context) error {
	if err := s.auth.Start(ctx); err != nil {
		return err
	}
	if err := s.engine.StartJobs(ctx); err != nil {
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
	// No server-wide ReadTimeout: it would also cancel a handler still
	// answering after it, so Handler bounds body reads instead.
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: cmp.Or(s.cfg.IdleTimeout, 120*time.Second),
		MaxHeaderBytes: maxHeaderBytes, ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)}
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

// Close stops the jobs and closes the engine, OpenRails' client and AuthKit,
// leaving the pool and Redis open.
func (s *Server) Close(ctx context.Context) error {
	var errs []error
	if s.engine != nil {
		errs = append(errs, s.engine.Close(ctx))
	}
	if s.billing != nil {
		errs = append(errs, s.billing.Close(ctx))
		s.billing = nil
	}
	if s.auth != nil {
		errs = append(errs, s.auth.Close(ctx))
	}
	if s.keys != nil {
		s.keys.Close()
	}
	return errors.Join(errs...)
}
