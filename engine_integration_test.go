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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/kindsroute"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/migrations"
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
	if g := e.RouteGroups(); g != ContentRoutes|CodeRoutes|GroupRoutes|NotificationRoutes|DiscoveryRoutes {
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
	routes, err := e.Routes(Routes{Prefix: "/api/ck/", Groups: ContentRoutes | CodeRoutes | GroupRoutes | NotificationRoutes | DiscoveryRoutes})
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
	s.call(http.StatusCreated, user, "POST", "/api/ck/v1/gallery/"+gallery+"/comments", map[string]string{"body": "first"}, nil)
	s.call(http.StatusOK, user, "POST", "/api/ck/v1/gallery/"+gallery+"/like", nil, nil)
	s.call(http.StatusOK, user, "POST", "/api/ck/v1/gallery/"+gallery+"/favorite", nil, nil)
	s.call(http.StatusUnauthorized, anon, "POST", "/api/ck/v1/gallery/"+gallery+"/favorite", nil, nil)
	s.call(http.StatusNotFound, user, "POST", "/api/ck/v1/gallery/"+cid(1)+"/like", nil, nil)
	s.call(http.StatusNotFound, user, "POST", "/api/ck/v1/video/"+gallery+"/like", nil, nil)
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
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/groups", []groups.GroupInput{{Kind: "tag", Slug: "moonlit",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Moonlit"}}}}, nil)
	s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/groups", []groups.GroupInput{{Kind: "tag", Slug: "nope"}}, nil)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/groups?kind=tag", nil, nil)
	s.call(http.StatusNotFound, anon, "GET", "/api/ck/v1/nowhere/at/all/x", nil, nil)

	// Discovery shares /v1 with content and groups; without a signal plane its
	// signal reads are not configured.
	var listed struct {
		Contents []json.RawMessage `json:"contents"`
	}
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/contents?kind=gallery", nil, &listed)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/search?q=moonlit", nil, nil)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/typeahead?q=moon", nil, nil)
	s.call(http.StatusOK, user, "GET", "/api/ck/v1/me/content-filters", nil, nil)
	s.call(http.StatusNotImplemented, user, "GET", "/api/ck/v1/me/history", nil, nil)
	s.call(http.StatusUnauthorized, anon, "GET", "/api/ck/v1/recommendations", nil, nil)

	// The public content model, without server-only parts.
	var model kindsroute.ContentModel
	rec := s.call(http.StatusOK, anon, "GET", "/api/ck/v1/kinds", nil, &model)
	if model.Kinds["gallery"].Route != "g" || model.GroupKinds["tag"].Route != "tag" || len(model.Languages) != 1 || model.Tiers == nil ||
		rec.Header().Get("Cache-Control") != "public, max-age=300" || strings.Contains(rec.Body.String(), "restrictions") {
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
	items, err := e.Client().PublishNotifications(ctx, []notify.Input{{Recipient: recipient, Origin: notify.Origin{System: "app", Scope: tenant, ID: "a"},
		Kind: "app.hello", CreatedAt: time.Now().UTC()}})
	if err != nil || len(items) != 1 {
		t.Fatalf("publish %+v %v", items, err)
	}
	var ce *Error
	if _, err := e.Client().PublishNotifications(ctx, nil); !errors.As(err, &ce) || ce.Code != "invalid_request" {
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
