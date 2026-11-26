package critical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/authkit/notify"
	notifypg "github.com/open-rails/authkit/notify/postgres"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
)

type clock struct {
	mu sync.RWMutex
	at time.Time
}

func (c *clock) now() time.Time   { c.mu.RLock(); defer c.mu.RUnlock(); return c.at }
func (c *clock) set(at time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.at = at }

// world contains real public runtimes. The only controlled dependency is time;
// AuthKit's Outbox is an external mail/SMS transport, not an identity mock.
type world struct {
	pool               *pgxpool.Pool
	auth               *authkit.Client
	roles              *authkit.Roles
	writer             iam.Role
	contributor        iam.Role
	submitter          iam.Role
	ownEditor          iam.Role
	viewer             iam.Role
	moderator          iam.Role
	commentManager     iam.Role
	importer           iam.Role
	locationManager    iam.Role
	globalReviewer     iam.Role
	channelWriter      iam.Role
	channelContributor iam.Role
	channelSubmitter   iam.Role
	channelOwnEditor   iam.Role
	channelModerator   iam.Role
	channelManager     iam.Role
	tenant             [2]*authkit.Client
	apps               []*application
	clock              *clock
	schema             string
	// authSchema holds AuthKit's tables, the account inbox among them.
	authSchema string
	signer     keys.Signer
	configure  []func(*ck.Config)
	// people are the accounts person signed in, by ID.
	people sync.Map
	// avatarBuckets are each tenant's avatar primary's bucket, by tenant ID.
	avatarBuckets sync.Map
	authStart     sync.Once
}

type application struct {
	test   *testing.T
	world  *world
	engine *ck.Engine
	cfg    ck.Config
	deps   ck.Deps
	group  iam.GroupRef
	server *httptest.Server
	tenant *authkit.Client
	// inbox is the app's inbox owner: its own deployment (issuer) on the
	// world's account store, so its app-visible notices are its alone.
	inbox         *authkit.Client
	inboxServer   *httptest.Server
	inboxStore    *notifypg.Store
	inboxSessions sync.Map
}

func newWorld(t *testing.T, configure ...func(*ck.Config)) *world {
	return newWorldAuth(t, nil, configure...)
}

func newWorldAuth(t *testing.T, configureAuth func(*authkit.Config), configure ...func(*ck.Config)) *world {
	t.Helper()
	dsn := os.Getenv("CONTENTKIT_CRITICAL_PG")
	if dsn == "" {
		t.Fatal("CONTENTKIT_CRITICAL_PG is required; integration cases never skip or use a default database")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	must(t, err)
	t.Cleanup(pool.Close)
	must(t, pool.Ping(ctx))
	_, err = pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS pgroonga")
	must(t, err)
	w := &world{pool: pool, clock: &clock{at: time.Now().UTC().Truncate(time.Second)}, schema: "ckc_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20], configure: configure}
	w.authSchema = w.schema + "_auth"
	// Registered before AuthKit's own cleanup, so it runs after AuthKit closes.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// The standalone server keeps AuthKit's River in <schema>_river.
		for _, schema := range []string{w.authSchema, w.authSchema + "_river"} {
			if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
				t.Errorf("drop AuthKit fixture schema: %v", err)
			}
		}
	})
	w.roles = authkit.NewRoles(authkit.APIKeys)
	appPersona := w.roles.Persona("app", authkit.APIKeys)
	permissions := map[access.Permission]iam.Perm{}
	for _, permission := range access.Permissions() {
		permissions[permission] = appPersona.Declare(ckauth.AppPermission(permission))[0]
	}
	write := permissions[access.ContentWrite]
	moderate := permissions[access.CommentsModerate]
	w.writer = appPersona.Role("writer", write)
	w.contributor = appPersona.Role("contributor", permissions[access.ContentCreate], permissions[access.ContentEditOwn])
	w.submitter = appPersona.Role("submitter", permissions[access.ContentCreate])
	w.ownEditor = appPersona.Role("own-editor", permissions[access.ContentEditOwn])
	w.viewer = appPersona.Role("auditor", permissions[access.ContentViewRestricted])
	w.moderator = appPersona.Role("moderator", moderate)
	w.commentManager = appPersona.Role("comment-manager", moderate, permissions[access.CommentsBan], permissions[access.ModerationReview])
	w.importer = appPersona.Role("importer", permissions[access.ImportWrite])
	w.locationManager = appPersona.Role("location-manager", permissions[access.LocationsWrite])
	w.globalReviewer = w.roles.Root.Role("reviewer", moderate)
	channel := w.roles.Persona("channel")
	channelPermissions := map[string]iam.Perm{}
	for _, permission := range access.ChannelPermissions() {
		channelPermissions[permission] = channel.Declare(permission)[0]
	}
	w.channelWriter = channel.Role("writer", channelPermissions["channel:content:write"])
	w.channelContributor = channel.Role("contributor", channelPermissions["channel:content:create"], channelPermissions["channel:content:edit-own"])
	w.channelSubmitter = channel.Role("submitter", channelPermissions["channel:content:create"])
	w.channelOwnEditor = channel.Role("own-editor", channelPermissions["channel:content:edit-own"])
	w.channelModerator = channel.Role("moderator", channelPermissions["channel:comments:moderate"])
	w.channelManager = channel.Role("manager", channelPermissions["channel:settings:manage"], channelPermissions["channel:media:write"])
	w.auth, _ = authtest.New(t,
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = w.roles
			c.Tenancy = authkit.TenancyMulti // tenants and their apps share one directory
			c.Database.Schema = w.authSchema
			authtest.LiftLimits(c) // accounts sign in at several apps' inbox owners
			if configureAuth != nil {
				configureAuth(c)
			}
		}),
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool; w.signer = d.KeySource.ActiveSigner() }))
	sessionIssuer.Store(w.auth)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, suffix := range []string{"_media_worker", "_river", ""} {
			_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema + suffix}.Sanitize()+" CASCADE")
			if err != nil {
				t.Errorf("drop owned fixture schema: %v", err)
			}
		}
	})
	for i := range w.tenant {
		tid := iam.NewTenantID()
		must(t, w.auth.CreateTenant(ctx, iam.Tenant{ID: tid, Slug: "ck-" + tid.String()}))
		w.tenant[i], err = w.auth.ForTenant(ctx, tid)
		must(t, err)
		for n := 0; n < 2; n++ {
			w.apps = append(w.apps, w.newApplication(t, w.tenant[i], tid))
		}
	}
	return w
}

func (w *world) newApplication(t *testing.T, tenant *authkit.Client, tid iam.TenantID) *application {
	t.Helper()
	ctx := t.Context()
	aid := ck.NewAppID()
	persona, err := w.auth.Persona("app")
	must(t, err)
	_, err = tenant.CreateGroup(ctx, iam.NewGroup{ID: aid.String(), TenantID: tid, Persona: persona})
	must(t, err)
	group := iam.GroupByID(aid.String())
	permissionScope, err := tenant.Scope(ctx, group)
	must(t, err)
	ctid, err := ck.ParseTenantID(tid.String())
	must(t, err)
	scope := ck.Scope{TenantID: ctid, AppID: aid}
	policy, err := ckauth.NewAuthorization(ctx, w.auth, ckauth.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: permissionScope})
	must(t, err)
	// All apps share tables so missing tenant/app predicates cannot hide
	// behind separately provisioned databases or schemas.
	cfg := ck.Config{Scope: &scope, Database: ck.DatabaseConfig{Schema: w.schema},
		Content: kinds.Config{Languages: []string{"en", "de"}, Tiers: []string{"gold"}, Channels: &kinds.Channels{Creation: "users", Route: "channels", MaxPerOwner: 1}, EntityKinds: map[string]kinds.EntityKind{}, Kinds: map[string]kinds.Kind{
			"article": {Channel: &kinds.ChannelPolicy{}, Route: "articles", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
			"gallery": {Route: "galleries", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Segments: true},
		}},
	}
	deps := ck.Deps{Postgres: w.pool, Auth: w.auth.Authenticator(), Authorization: policy, Users: tenant.UserInfo(), Clock: w.clock.now}
	for _, configure := range w.configure {
		configure(&cfg)
	}
	// Each app's inbox owner is a sibling deployment on the world's account
	// store under its own issuer: AuthKit shows a deployment its own
	// app-visible notices and every tenant-wide one.
	issuer := "https://" + aid.String() + ".apps.example.com"
	inboxOwner := authtest.Replica(t, w.auth, authtest.WithConfig(func(c *authkit.Config) { c.Issuer = issuer }))
	inbox, err := inboxOwner.ForTenant(ctx, tid)
	must(t, err)
	deps.Notifications, err = inbox.Notifications(authkit.NotificationProducer{Name: "contentkit", Origin: notify.OriginScope{System: "contentkit", Scope: aid.String()}})
	must(t, err)
	inboxScope, err := inbox.NotificationScope()
	must(t, err)
	inboxStore, err := notifypg.New(w.pool, notifypg.Config{Schema: w.authSchema, Scope: inboxScope, AppView: &notify.AppView{AppID: issuer}})
	must(t, err)
	inboxServer := httptest.NewServer(inboxOwner.Handler())
	t.Cleanup(inboxServer.Close)
	deps.Scopes, err = ckauth.AppScopes(w.auth, scope)
	must(t, err)
	engine, err := ck.New(ctx, cfg, deps)
	must(t, err)
	a := &application{test: t, world: w, engine: engine, cfg: cfg, deps: deps, group: group, tenant: tenant, inbox: inbox, inboxServer: inboxServer, inboxStore: inboxStore}
	routes, err := engine.Routes(ck.Routes{Prefix: "/content", Groups: engine.RouteGroups()})
	must(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Path, route.Handler)
	}
	a.server = httptest.NewServer(withSessionProofs(t, mux))
	t.Cleanup(func() {
		a.server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := a.engine.Close(ctx); err != nil {
			t.Errorf("close ContentKit: %v", err)
		}
	})
	return a
}

// bearerTransport presents token as a signed-in browser does: a session
// SignIn bound to a DPoP key with a fresh proof naming auth's origin
// (authtest.Authorize), an API key or any other token as Bearer. Without auth
// every token goes as Bearer.
type bearerTransport struct {
	test  testing.TB
	auth  *authkit.Client
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	presentToken(b.test, b.auth, r, b.token)
	return b.base.RoundTrip(r)
}

func presentToken(t testing.TB, auth *authkit.Client, r *http.Request, token string) {
	switch {
	case token == "":
	case auth == nil:
		r.Header.Set("Authorization", "Bearer "+token)
	default:
		authtest.Authorize(t, auth, r, token)
	}
}

// sessionIssuer is an AuthKit client of the suite's issuer (authtest.Issuer).
// authtest.Authorize reads only its public origin, and authtest keeps every
// SignIn session's DPoP key for the process, so any world's client presents
// any world's sessions.
var sessionIssuer atomic.Pointer[authkit.Client]

// withSessionProofs fronts a ContentKit test server as a browser's own
// transport would: a Bearer token a browser or Node driver sent arrives as
// authtest.Authorize presents it, a session SignIn bound to a DPoP key with
// its proof. Only this process holds the session keys. Any other credential,
// and a request already carrying a proof, passes unchanged.
func withSessionProofs(t testing.TB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") && r.Header.Get("DPoP") == "" {
			if auth := sessionIssuer.Load(); auth != nil {
				r = r.Clone(r.Context())
				authtest.Authorize(t, auth, r, token)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *application) client(token string) *ck.Client {
	return ck.NewClient(a.server.URL+"/content", &http.Client{Transport: bearerTransport{test: a.test, auth: a.tenant, token: token, base: http.DefaultTransport}})
}

func (w *world) person(t *testing.T, tenant int) (authtest.User, string) {
	t.Helper()
	user := authtest.NewUser(t, w.tenant[tenant])
	w.people.Store(user.ID, user)
	return user, authtest.SignIn(t, w.tenant[tenant], user).AccessToken
}

// startAuth runs AuthKit's own River once, for its account purges.
func (w *world) startAuth(t *testing.T) {
	t.Helper()
	w.authStart.Do(func() { must(t, w.auth.Start(context.Background())) })
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func publishedArticle(t *testing.T, a *application, title string) catalog.Content {
	t.Helper()
	startPublicationRuntime(t, a)
	item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title, Body: "Published body: " + title}}}}})
	must(t, err)
	item, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	var got catalog.Content
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, a)
		got, err = a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		return err == nil && got.Live
	})
	if got.ID != item.ID || got.Body != "Published body: "+title || !got.Decision.Allowed {
		t.Fatalf("fixture was not publicly readable: %+v", got)
	}
	item, err = a.engine.Client().GetContent(t.Context(), item.ID, ck.GetContentQuery{})
	must(t, err)
	return item
}

// Tests that request a published fixture exercise its real durable workers.
// newWorld itself remains unstarted for lifecycle and clock-only refusal tests.
func startPublicationRuntime(t *testing.T, a *application) {
	t.Helper()
	if err := a.engine.Ready(t.Context()); err != nil {
		if err.Error() != "contentkit: not started" {
			t.Fatal(err)
		}
		must(t, a.engine.StartJobs(t.Context()))
	}
}
