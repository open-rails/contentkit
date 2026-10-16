package contentkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/notify"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/kindsroute"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/contentkit/signal"
)

const tenant = "site"

// cid is a deterministic canonical UUIDv7 content id.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func ptr[T any](v T) *T { return &v }

// staffAuthz grants every permission to the actor "staff" and none to anyone else.
type staffAuthz struct{}

func (staffAuthz) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff" && !a.Anonymous, nil
}

// engineSchema names a disposable schema and drops it, its River and media
// worker schemas and their ledger rows afterwards.
func engineSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	pgtest.EnsureExtensions(t, t.Context(), pool)
	s := "ck_e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range []string{s, s + "_river", s + "_media_worker"} {
			_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE")
		}
		_, _ = pool.Exec(ctx, "DELETE FROM public.migrations WHERE schema = $1", s)
	})
	return s
}

func siteConfig(schema string) Config {
	return Config{Tenant: tenant, Database: DatabaseConfig{Schema: schema},
		Content: kinds.Config{Languages: []string{"en"}, Restrictions: []string{"restricted"},
			Kinds: map[string]kinds.Kind{
				"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}},
				"blog":    {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
			},
			GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tag"}}},
		Interactions: InteractionsConfig{Limits: LimitsConfig{Disabled: true}}}
}

// server mounts routes on one root mux, as a host does.
type server struct {
	t *testing.T
	h *http.ServeMux
}

func serve(t *testing.T, routes []Route) server {
	mux := http.NewServeMux()
	for _, r := range routes {
		pattern := r.Path
		if r.Method != "" {
			pattern = r.Method + " " + r.Path
		}
		mux.Handle(pattern, r.Handler)
	}
	return server{t, mux}
}

func (s server) call(status int, actor access.Actor, method, target string, body, out any) *httptest.ResponseRecorder {
	s.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			s.t.Fatal(err)
		}
	}
	req := testauth.As(httptest.NewRequest(method, target, &buf).WithContext(s.t.Context()), actor)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != status {
		s.t.Fatalf("%s %s as %q: %d, want %d: %s", method, target, actor.ID, rec.Code, status, rec.Body)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			s.t.Fatalf("%s %s: %v", method, target, err)
		}
	}
	return rec
}

// One constructor migrates ContentKit's own schema (concurrently, twice), the
// routes serve every module under /v1, content is created and interacted
// with through them, Start runs the engine's jobs on its own River, which
// index content items and tags, and the in-process client publishes as the
// host.
func TestEngineIntegration(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := siteConfig(schema)
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}}

	engines := make(chan *Engine, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			e, err := New(ctx, cfg, deps)
			engines <- e
			errs <- err
		}()
	}
	var e *Engine
	for range 2 {
		got := <-engines
		if err := <-errs; err != nil {
			t.Fatalf("concurrent first New: %v", err)
		}
		if e == nil {
			e = got
		} else {
			_ = got.Close(ctx)
		}
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	for name, edit := range map[string]func(*Config, *Deps){
		"no tenant": func(c *Config, _ *Deps) { c.Tenant = "" },
		"reserved kind": func(c *Config, _ *Deps) {
			c.Content.Kinds["media"] = kinds.Kind{Route: "m", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}}
		},
		"no auth": func(_ *Config, d *Deps) { d.Auth = nil },
		"media hooks": func(c *Config, d *Deps) {
			c.Media = &MediaConfig{Config: media.Config{Hooks: media.Hooks{PurgePublic: func(context.Context, []string) {}}}}
			d.Store = nil
		},
		"bad schema": func(c *Config, _ *Deps) { c.Database.Schema = "Has-Dash" },
	} {
		c, d := siteConfig(schema), deps
		edit(&c, &d)
		if _, err := New(ctx, c, d); err == nil {
			t.Fatalf("%s: New accepted it", name)
		}
	}

	chain, err := fs.Glob(migrations.Postgres, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM public.migrations WHERE app = 'contentkit' AND schema = $1", schema).Scan(&applied); err != nil || applied != len(chain) {
		t.Fatalf("ledger rows %d, want %d: %v", applied, len(chain), err)
	}
	if g := e.RouteGroups(); g != ContentRoutes|CodeRoutes|EntityRoutes|NotificationRoutes|DiscoveryRoutes|ImportRoutes {
		t.Fatalf("route groups %b", g)
	}
	if _, err := e.Routes(Routes{Prefix: "/api/ck"}); err == nil {
		t.Fatal("Routes without groups")
	}
	for _, g := range []RouteGroups{MediaRoutes, UploadRoutes} {
		if _, err := e.Routes(Routes{Prefix: "/api/ck", Groups: g}); err == nil {
			t.Fatalf("group %d served without its module", g)
		}
	}
	routes, err := e.Routes(Routes{Prefix: "/api/ck/", Groups: ContentRoutes | CodeRoutes | EntityRoutes | NotificationRoutes | DiscoveryRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user", IP: "10.0.0.1"}, access.Actor{Anonymous: true, IP: "10.0.0.2"}

	// Content is created through the catalog's routes; interactions on it
	// sit beside them under /v1.
	create := func(in catalog.ContentInput) string {
		t.Helper()
		var c catalog.Content
		s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/contents", in, &c)
		s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
		return c.ID
	}
	gallery := create(catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Harbor"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}}})
	create(catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Autumn Festival Report", Body: "b"}}})
	s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/contents", catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "No"}}}, nil)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/contents/"+gallery, nil, nil)
	s.call(http.StatusCreated, user, "POST", "/api/ck/v1/contents/"+gallery+"/comments", map[string]string{"body": "first"}, nil)
	s.call(http.StatusOK, user, "POST", "/api/ck/v1/contents/"+gallery+"/like", nil, nil)
	s.call(http.StatusOK, user, "POST", "/api/ck/v1/contents/"+gallery+"/favorite", nil, nil)
	s.call(http.StatusUnauthorized, anon, "POST", "/api/ck/v1/contents/"+gallery+"/favorite", nil, nil)
	s.call(http.StatusNotFound, user, "POST", "/api/ck/v1/contents/"+cid(1)+"/like", nil, nil)
	s.call(http.StatusNotFound, user, "POST", "/api/ck/v1/entities/"+gallery+"/like", nil, nil)
	g1 := contentref.New(tenant, "gallery", gallery)
	counts, err := e.content.Counts(ctx, []contentref.ContentRef{g1})
	if err != nil || counts[g1.Key()] != (content.Counts{Likes: 1, Favorites: 1, CommentCount: 1}) {
		t.Fatalf("counts %+v %v", counts, err)
	}
	var totals catalog.Totals
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/catalog/totals?kinds=gallery,blog", nil, &totals)
	if totals.Kinds["gallery"].Public != 1 || totals.Kinds["blog"].Public != 1 {
		t.Fatalf("totals %+v", totals)
	}
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/entities", []groups.GroupInput{{Kind: "tag", Slug: "moonlit",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Moonlit"}}}}, nil)
	s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/entities", []groups.GroupInput{{Kind: "tag", Slug: "nope"}}, nil)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/entities?kind=tag", nil, nil)
	s.call(http.StatusNotFound, anon, "GET", "/api/ck/v1/nowhere/at/all/x", nil, nil)

	// Discovery shares /v1 with content and groups; without a signal plane its
	// signal reads are not configured.
	var listed struct {
		Contents []json.RawMessage `json:"contents"`
	}
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/contents?kind=gallery", nil, &listed)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/search?q=moonlit", nil, nil)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/typeahead?q=moon", nil, nil)
	s.call(http.StatusOK, user, "GET", "/api/ck/v1/content-filters", nil, nil)
	s.call(http.StatusNotImplemented, user, "GET", "/api/ck/v1/me/history", nil, nil)
	s.call(http.StatusUnauthorized, anon, "GET", "/api/ck/v1/recommendations", nil, nil)

	// The public content model, without server-only parts.
	var model kindsroute.ContentModel
	rec := s.call(http.StatusOK, anon, "GET", "/api/ck/v1/kinds", nil, &model)
	if model.Kinds["gallery"].Route != "g" || model.GroupKinds["tag"].Route != "tag" || len(model.Languages) != 1 || model.Tiers == nil || model.Signals ||
		!strings.HasPrefix(rec.Header().Get("Cache-Control"), "public, max-age=") || strings.Contains(rec.Body.String(), "restrictions") {
		t.Fatalf("kinds %d %s", rec.Code, rec.Body)
	}

	if err := e.Ready(ctx); err == nil {
		t.Fatal("Ready before Start")
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx); err == nil {
		t.Fatal("Start twice")
	}
	if err := e.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	// The search job runs on start and indexes ContentKit's own documents.
	deadline := time.Now().Add(90 * time.Second)
	for _, c := range []struct{ q, kind string }{{"autumn festival", "blog"}, {"moonlit", "tag"}} {
		for {
			res, err := e.hub.Search(ctx, c.q, hub.HubSearchOptions{SearchOptions: hub.SearchOptions{Language: "en", ContentKinds: []string{c.kind}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Hits) == 1 && res.Hits[0].ContentKind == c.kind {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("search %q never found the %s", c.q, c.kind)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// The host publishes notices in process; people read their inbox over HTTP.
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: "u1"}
	published, err := e.Client().PostNotificationsPublish(ctx, notification.Publication{Notifications: []notify.Input{{Recipient: recipient,
		Origin: notify.Origin{System: "app", Scope: tenant, ID: "a"}, Kind: "app.hello", CreatedAt: time.Now().UTC()}}})
	items := published.Items
	if err != nil || len(items) != 1 {
		t.Fatalf("publish %+v %v", items, err)
	}
	var ce *Error
	if _, err := e.Client().PostNotificationsPublish(ctx, notification.Publication{}); !errors.As(err, &ce) || ce.Code != "invalid_request" {
		t.Fatalf("empty publish: %v", err)
	}
	s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/notifications/publish", map[string]any{"notifications": []notify.Input{}}, nil)
	var page notify.Page
	s.call(http.StatusOK, user, "GET", "/api/ck/v1/notifications/inbox", nil, &page)
	if len(page.Items) != 1 || page.Items[0].ID != items[0].ID {
		t.Fatalf("inbox %+v", page)
	}
	report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: "u1"}})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure %+v %v", report, err)
	}
	s.call(http.StatusOK, user, "GET", "/api/ck/v1/notifications/inbox", nil, &page)
	if len(page.Items) != 0 {
		t.Fatalf("erased inbox %+v", page)
	}

	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Ready(ctx); err == nil {
		t.Fatal("Ready after Close")
	}
}

// Without ClickHouse, or with one that is down, the engine runs on Postgres
// alone: its content model says signals are off, lists (popular too),
// details and search serve, and signals, exposures and history answer
// not_configured, or unavailable while a configured plane is down, which
// fails its probe and is retried in the background.
func TestEngineWithoutSignalPlaneIntegration(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	dead := newCHProxy(t, "")
	for name, ch := range map[string]ClickHouseConfig{"none": {}, "down": {Addr: dead.addr(), Database: "signals"}} {
		t.Run(name, func(t *testing.T) {
			cfg := siteConfig(engineSchema(t, pool))
			cfg.ClickHouse = ch
			e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = e.Close(context.Background()) })
			probes := e.Probes()
			if down := ch.Addr != ""; down != (len(probes) == 1) || down && (probes[0].Name != "contentkit_clickhouse" || probes[0].Check(ctx) == nil) {
				t.Fatalf("probes %+v", probes)
			}
			if err := e.Start(ctx); err != nil {
				t.Fatal(err)
			}
			routes, err := e.Routes(Routes{Prefix: "/ck", Groups: ContentRoutes | DiscoveryRoutes})
			if err != nil {
				t.Fatal(err)
			}
			s := serve(t, routes)
			staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true}

			var model kindsroute.ContentModel
			if s.call(http.StatusOK, anon, "GET", "/ck/v1/kinds", nil, &model); model.Signals {
				t.Fatal("the content model says signals are recorded")
			}
			var c catalog.Content
			s.call(http.StatusCreated, staff, "POST", "/ck/v1/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Harbor Lights"}},
				Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &c)
			s.call(http.StatusOK, staff, "POST", "/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
			var detail catalog.Content
			if s.call(http.StatusOK, anon, "GET", "/ck/v1/contents/"+c.ID, nil, &detail); detail.Title != "Harbor Lights" || !detail.Live {
				t.Fatalf("detail %+v", detail)
			}
			for _, sort := range []string{"newest", "popular"} {
				var page struct {
					Contents []struct{ ID string } `json:"contents"`
				}
				if s.call(http.StatusOK, anon, "GET", "/ck/v1/contents?kind=gallery&sort="+sort, nil, &page); len(page.Contents) != 1 || page.Contents[0].ID != c.ID {
					t.Fatalf("%s list %+v", sort, page)
				}
			}
			for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(300 * time.Millisecond) {
				var found struct {
					Contents []struct{ ID string } `json:"contents"`
				}
				if s.call(http.StatusOK, anon, "GET", "/ck/v1/search?q=harbor&language=en", nil, &found); len(found.Contents) == 1 && found.Contents[0].ID == c.ID {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("search never found the gallery")
				}
			}
			status, code := http.StatusNotImplemented, `"not_configured"`
			if ch.Addr != "" {
				status, code = http.StatusServiceUnavailable, `"unavailable"`
			}
			off := func(rec *httptest.ResponseRecorder) {
				t.Helper()
				if !strings.Contains(rec.Body.String(), code) {
					t.Fatalf("%s, want %s", rec.Body, code)
				}
			}
			now := time.Now().UTC()
			off(s.call(status, user, "POST", "/ck/v1/exposures", discovery.Exposures{Exposures: []discovery.ExposureInput{{RenderID: "r1", Stage: "rendered",
				OccurredAt: &now, Shown: []discovery.Placement{{ContentID: c.ID, Position: 1}}}}}, nil))
			off(s.call(status, user, "POST", "/ck/v1/signals", discovery.Signals{Signals: []discovery.SignalInput{{ContentID: c.ID, Type: "click",
				EventID: uuid.NewString(), OccurredAt: now}}}, nil))
			off(s.call(status, user, "GET", "/ck/v1/me/history", nil, nil))
			// A plane that is down may still hold the subject's signals: erasure stays pending.
			report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: "u1"}})
			if down := ch.Addr != ""; down == (err == nil) || down == report.Complete() || down != (report.Remaining["signal_plane"] == 1) {
				t.Fatalf("erasure %+v %v", report, err)
			}
			if ch.Addr != "" {
				// The engine keeps retrying in the background.
				seen := dead.accepted.Load()
				for deadline := time.Now().Add(20 * time.Second); dead.accepted.Load() < seen+2; time.Sleep(100 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("no retries: %d connections", dead.accepted.Load())
					}
				}
			}
		})
	}
}

// On a host's fleet the engine's jobs run on the host's River client, which
// must be the one built with its RiverJobs.
func TestEngineOnHostFleet(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := siteConfig(schema)
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	fleet, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema + "_river", FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond},
		e.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx); err == nil {
		t.Fatal("own River started after RiverJobs went to a fleet")
	}
	if err := e.Start(ctx, WithRiverClient(nil)); err == nil {
		t.Fatal("nil fleet accepted")
	}
	events, cancel := fleet.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if err := fleet.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.StopAndCancel(context.Background()) })
	if err := e.Start(ctx, WithRiverClient(fleet)); err != nil {
		t.Fatal(err)
	}
	if err := e.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Insert(ctx, searchSyncArgs{}, &river.InsertOpts{Queue: QueueContentKit}); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(60 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Job.Kind == (searchSyncArgs{}).Kind() {
				return
			}
		case <-timeout:
			t.Fatal("the search job never completed on the fleet")
		}
	}
}

func pngBytes(t *testing.T, w int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, w))
	for i := range w {
		img.Set(i, i, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// chProxy stands between an engine and ClickHouse: down, it hangs up on
// every connection, as a ClickHouse that is down does.
type chProxy struct {
	ln       net.Listener
	target   string
	accepted atomic.Int64
	mu       sync.Mutex
	up       bool
	conns    []net.Conn
}

func newCHProxy(t *testing.T, target string) *chProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &chProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.set(false)
	})
	return p
}

func (p *chProxy) addr() string { return p.ln.Addr().String() }

func (p *chProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		go p.pipe(c)
	}
}

func (p *chProxy) pipe(c net.Conn) {
	var s net.Conn
	p.mu.Lock()
	if p.up {
		s, _ = net.Dial("tcp", p.target)
	}
	if s == nil {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.conns = append(p.conns, c, s)
	p.mu.Unlock()
	go func() {
		_, _ = io.Copy(s, c)
		_ = s.Close()
	}()
	_, _ = io.Copy(c, s)
	_ = c.Close()
}

// set brings ClickHouse up or down; down drops every open connection.
func (p *chProxy) set(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = up
	if !up {
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	}
}
