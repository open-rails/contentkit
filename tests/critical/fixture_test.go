package critical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
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
	pool             *pgxpool.Pool
	auth             *authkit.Client
	roles            *authkit.Roles
	writer           iam.Role
	viewer           iam.Role
	moderator        iam.Role
	commentManager   iam.Role
	globalReviewer   iam.Role
	channelWriter    iam.Role
	channelModerator iam.Role
	tenant           [2]*authkit.Client
	apps             []*application
	clock            *clock
	schema           string
	signer           keys.Signer
	configure        []func(*ck.Config)
}

type application struct {
	engine *ck.Engine
	cfg    ck.Config
	deps   ck.Deps
	group  iam.GroupRef
	server *httptest.Server
	tenant *authkit.Client
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
	w.roles = authkit.NewRoles(authkit.APIKeys)
	appPersona := w.roles.Persona("app", authkit.APIKeys)
	permissions := map[access.Permission]iam.Perm{}
	for _, permission := range access.Permissions() {
		permissions[permission] = appPersona.Declare(ckauth.AppPermission(permission))[0]
	}
	write := permissions[access.ContentWrite]
	moderate := permissions[access.CommentsModerate]
	w.writer = appPersona.Role("writer", write)
	w.viewer = appPersona.Role("auditor", permissions[access.ContentViewRestricted])
	w.moderator = appPersona.Role("moderator", moderate)
	w.commentManager = appPersona.Role("comment-manager", moderate, permissions[access.CommentsBan], permissions[access.ModerationReview])
	w.globalReviewer = w.roles.Root.Role("reviewer", moderate)
	channel := w.roles.Persona("channel")
	channelPermissions := map[string]iam.Perm{}
	for _, permission := range access.EntityPermissions("channel") {
		channelPermissions[permission] = channel.Declare(permission)[0]
	}
	w.channelWriter = channel.Role("writer", channelPermissions["channel:content:write"])
	w.channelModerator = channel.Role("moderator", channelPermissions["channel:comments:moderate"])
	w.auth, _ = authtest.New(t,
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = w.roles
			c.DefaultTenantID = iam.TenantID{}
			if configureAuth != nil {
				configureAuth(c)
			}
		}),
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool; w.signer = d.KeySource.ActiveSigner() }))
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
		must(t, w.auth.CreateTenant(ctx, iam.Tenant{ID: tid}))
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
		Content: kinds.Config{Languages: []string{"en", "de"}, Tiers: []string{"gold"}, EntityKinds: map[string]kinds.EntityKind{"channel": {Route: "channels", Owner: "user", AuthScope: true, MaxPerOwner: 1}}, Kinds: map[string]kinds.Kind{
			"article": {Route: "articles", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
			"gallery": {Route: "galleries", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Segments: true},
		}},
	}
	deps := ck.Deps{Postgres: w.pool, Auth: w.auth.Authenticator(), Authorization: policy, Users: tenant.UserInfo(), Clock: w.clock.now}
	for _, configure := range w.configure {
		configure(&cfg)
	}
	deps.Scopes, err = ckauth.AppScopes(w.auth, scope)
	must(t, err)
	engine, err := ck.New(ctx, cfg, deps)
	must(t, err)
	a := &application{engine: engine, cfg: cfg, deps: deps, group: group, tenant: tenant}
	routes, err := engine.Routes(ck.Routes{Prefix: "/content", Groups: engine.RouteGroups()})
	must(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Path, route.Handler)
	}
	a.server = httptest.NewServer(mux)
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

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

func (a *application) client(token string) *ck.Client {
	return ck.NewClient(a.server.URL+"/content", &http.Client{Transport: bearerTransport{token, http.DefaultTransport}})
}

func (w *world) person(t *testing.T, tenant int) (authtest.User, string) {
	t.Helper()
	user := authtest.NewUser(t, w.tenant[tenant])
	return user, authtest.SignIn(t, w.tenant[tenant], user).AccessToken
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func publishedArticle(t *testing.T, a *application, title string) catalog.Content {
	t.Helper()
	item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: title, Body: "Published body: " + title}}})
	must(t, err)
	item, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	got, err := a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
	must(t, err)
	if got.ID != item.ID || got.Body != "Published body: "+title || !got.Decision.Allowed {
		t.Fatalf("fixture was not publicly readable: %+v", got)
	}
	return item
}
