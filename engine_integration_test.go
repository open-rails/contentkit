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
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/signal"
	"github.com/open-rails/contentkit/taxonomy"
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

// galleries is the host's resolver for the galleries it still owns: cid(1)
// is visible to everyone.
type galleries struct{}

func (galleries) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if r.ContentKind == "gallery" && r.ContentID == cid(1) {
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true, Owner: "owner"}
		}
	}
	return out, nil
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
		Content: kinds.Config{Languages: []string{"en"},
			Kinds: map[string]kinds.Kind{
				"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}},
				"post":    {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
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

func (s server) call(status int, actor access.Actor, method, target string, body, out any) {
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
}

// One constructor migrates ContentKit's own schema (concurrently, twice), the
// routes serve every module under /v1, Start runs the engine's jobs on its own
// River, which index ContentKit's posts and tags, and the in-process client
// publishes as the host.
func TestEngineIntegration(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := siteConfig(schema)
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Resolver: galleries{}}

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
			c.Media = &MediaConfig{Config: media.Config{Hooks: media.Hooks{Resolver: galleries{}}}}
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

	if g := e.RouteGroups(); g != ContentRoutes|CodeRoutes|TaxonomyRoutes|NotificationRoutes {
		t.Fatalf("route groups %b", g)
	}
	if _, err := e.Routes(Routes{Prefix: "/api/ck"}); err == nil {
		t.Fatal("Routes without groups")
	}
	for _, g := range []RouteGroups{MediaRoutes, UploadRoutes, CatalogRoutes} {
		if _, err := e.Routes(Routes{Prefix: "/api/ck", Groups: g}); err == nil {
			t.Fatalf("group %d served without its module", g)
		}
	}
	routes, err := e.Routes(Routes{Prefix: "/api/ck/", Groups: ContentRoutes | CodeRoutes | TaxonomyRoutes | NotificationRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user", IP: "10.0.0.1"}, access.Actor{Anonymous: true, IP: "10.0.0.2"}

	// Interactions on the host's gallery and on ContentKit's own posts, under /v1.
	s.call(http.StatusCreated, user, "POST", "/api/ck/v1/gallery/"+cid(1)+"/comments", map[string]string{"body": "first"}, nil)
	s.call(http.StatusOK, user, "POST", "/api/ck/v1/gallery/"+cid(1)+"/like", nil, nil)
	s.call(http.StatusOK, user, "POST", "/api/ck/v1/gallery/"+cid(1)+"/favorite", nil, nil)
	s.call(http.StatusUnauthorized, anon, "POST", "/api/ck/v1/gallery/"+cid(1)+"/favorite", nil, nil)
	s.call(http.StatusNotFound, user, "POST", "/api/ck/v1/video/"+cid(1)+"/like", nil, nil)
	g1 := contentref.New(tenant, "gallery", cid(1))
	counts, err := e.content.Counts(ctx, []contentref.ContentRef{g1})
	if err != nil || counts[g1.Key()] != (content.Counts{Likes: 1, Favorites: 1, CommentCount: 1}) {
		t.Fatalf("counts %+v %v", counts, err)
	}
	var post content.Post
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/posts", content.PostInput{Title: ptr("Autumn Festival Report"), Body: ptr("b"), Language: ptr("en"), IsDraft: ptr(false)}, &post)
	s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/posts", content.PostInput{Title: ptr("No")}, nil)
	if _, err := e.taxonomy.CreateNodes(ctx, []taxonomy.NodeInput{{Kind: "tag", Slug: "moonlit", Names: []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Moonlit"}}}}); err != nil {
		t.Fatal(err)
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
	for _, c := range []struct{ q, kind string }{{"autumn festival", content.KindPost}, {"moonlit", "tag"}} {
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
