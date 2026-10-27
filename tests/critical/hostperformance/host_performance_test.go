//go:build hostperformance

// Package hostperformance runs an explicitly selected disposable benchmark.
// It is excluded from ordinary critical qualification and retains its datasets.
package hostperformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	rh "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"go.yaml.in/yaml/v3"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func number(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil {
			panic(e)
		}
		return n
	}
	return def
}
func save(t testing.TB, path string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	must(t, e)
	must(t, os.WriteFile(path, append(b, '\n'), 0600))
}
func ident(prefix, n int) string { return fmt.Sprintf("%08x-0000-7000-8000-%012x", prefix, n) }

type modelConfig struct {
	Languages struct {
		Supported []string `yaml:"supported"`
		Default   string   `yaml:"default"`
	} `yaml:"languages"`
	Supported []string             `yaml:"supported_languages"`
	Default   string               `yaml:"default_language"`
	Filters   kinds.ContentFilters `yaml:"content_filters"`
}
type loggedCH struct {
	driver.Conn
	prefix  string
	seq     atomic.Uint64
	phase   atomic.Value
	mu      sync.Mutex
	queries []map[string]any
}

func (c *loggedCH) context(ctx context.Context, q string) context.Context {
	id := fmt.Sprintf("%s_%s_%d", c.prefix, c.phase.Load(), c.seq.Add(1))
	c.mu.Lock()
	c.queries = append(c.queries, map[string]any{"id": id, "phase": c.phase.Load(), "sql": q})
	c.mu.Unlock()
	return clickhouse.Context(ctx, clickhouse.WithQueryID(id))
}
func (c *loggedCH) Exec(ctx context.Context, q string, args ...any) error {
	return c.Conn.Exec(c.context(ctx, q), q, args...)
}
func (c *loggedCH) Query(ctx context.Context, q string, args ...any) (driver.Rows, error) {
	return c.Conn.Query(c.context(ctx, q), q, args...)
}

type fixture struct {
	readerSignIn                       func() string
	readerToken                        string
	pg                                 *pgTrace
	t                                  *testing.T
	app, kind, schema, db, tenant, out string
	items, users, days                 int
	model                              kinds.Config
	pool                               *pgxpool.Pool
	ch                                 *loggedCH
	engine                             *ck.Engine
	fleet                              *river.Client[pgx.Tx]
	server                             *httptest.Server
	now, day                           time.Time
	template                           catalog.Content
	tagIDs                             map[string]string
	phases                             []map[string]any
}

func TestHostPerformance(t *testing.T) {
	out := os.Getenv("HOST_PERF_OUTPUT")
	if out == "" {
		t.Fatal("HOST_PERF_OUTPUT must name a new durable output directory")
	}
	must(t, os.Mkdir(out, 0700))
	apps := strings.Split(os.Getenv("HOST_PERF_APPS"), ",")
	if apps[0] == "" {
		apps = []string{"doujins", "hentai0"}
	}
	for _, app := range apps {
		t.Run(app, func(t *testing.T) {
			f := newFixture(t, app, filepath.Join(out, app))
			defer f.record()
			f.seedCatalog()
			f.seedRawDay()
			f.refresh("raw_day")
			f.verifyDaily()
			f.httpChecks("raw_day_http")
			f.refresh("finalized_reuse")
			if f.days > 1 {
				f.seedHistory()
				f.refresh("historical_presets")
				f.httpChecks("historical_http")
				if os.Getenv("HOST_PERF_PROFILE") != "representative" {
					f.duringRefresh()
				}
				f.combinedRecompute()
			}
		})
	}
}
func newFixture(t *testing.T, app, out string) *fixture {
	must(t, os.Mkdir(out, 0700))
	f := &fixture{t: t, app: app, out: out, items: number("HOST_PERF_ITEMS", 1000), users: number("HOST_PERF_USERS", 600), days: number("HOST_PERF_DAYS", 1), tagIDs: map[string]string{}}
	if f.items < 100 || f.items > 100000 || f.users < 60 || f.users > 60000 {
		t.Fatal("bounded fixture: 100..100k items,60..60k users")
	}
	f.kind = "gallery"
	if app == "hentai0" {
		f.kind = "video"
	} else if app != "doujins" {
		t.Fatal("unknown app")
	}
	f.schema = "ck261hp_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	f.db = f.schema
	f.day = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	f.now = f.day.Add(23 * time.Hour)
	root := "../../../benchmarks/host-performance/models"
	b, e := os.ReadFile(filepath.Join(root, app+"-model.yaml"))
	must(t, e)
	must(t, yaml.Unmarshal(b, &f.model))
	b, e = os.ReadFile(filepath.Join(root, app+"-host.yaml"))
	must(t, e)
	var h modelConfig
	must(t, yaml.Unmarshal(b, &h))
	f.model.Languages = h.Languages.Supported
	if app == "hentai0" {
		f.model.Languages = h.Supported
	}
	f.model.ContentFilters = h.Filters
	var scopes []kinds.PopularityScope
	for _, d := range h.Filters.Domains {
		for _, l := range append([]string{""}, f.model.Languages...) {
			scopes = append(scopes, kinds.PopularityScope{Domain: d.Name, Language: l})
		}
	}
	k := f.model.Kinds[f.kind]
	for i := range k.Popularity {
		k.Popularity[i].Scopes = scopes
	}
	f.model.Kinds[f.kind] = k
	must(t, f.model.Validate())
	save(t, filepath.Join(out, "model.json"), f.model)
	f.ch = &loggedCH{prefix: f.schema}
	f.ch.phase.Store("setup")
	f.pg = &pgTrace{phase: &f.ch.phase}
	cfg, e := pgxpool.ParseConfig(os.Getenv("CONTENTKIT_CRITICAL_PG"))
	must(t, e)
	// Each app gets its own logical database on the existing fixture cluster;
	// PGroonga catalog/vacuum state from other owners cannot enter this run.
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	must(t, err)
	database := f.schema + "_db"
	_, err = adminPool.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
	adminPool.Close()
	must(t, err)
	cfg.ConnConfig.Database = database
	cfg.MaxConns = 12
	cfg.ConnConfig.Tracer = f.pg
	f.pool, e = pgxpool.NewWithConfig(t.Context(), cfg)
	must(t, e)
	t.Cleanup(f.pool.Close)
	_, e = f.pool.Exec(t.Context(), "CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS pgroonga")
	must(t, e)
	roles := authkit.NewRoles(authkit.APIKeys)
	persona := roles.Persona("app", authkit.APIKeys)
	for _, p := range access.Permissions() {
		persona.Declare(ckauth.AppPermission(p))
	}
	channel := roles.Persona("channel")
	for _, p := range access.ChannelPermissions() {
		channel.Declare(p)
	}
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles; c.DefaultTenantID = iam.TenantID{} }), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }))
	tid := iam.NewTenantID()
	must(t, ak.CreateTenant(t.Context(), iam.Tenant{ID: tid}))
	ta, e := ak.ForTenant(t.Context(), tid)
	must(t, e)
	aid := ck.NewAppID()
	ap, e := ak.Persona("app")
	must(t, e)
	_, e = ta.CreateGroup(t.Context(), iam.NewGroup{ID: aid.String(), TenantID: tid, Persona: ap})
	must(t, e)
	scopeAuth, e := ta.Scope(t.Context(), iam.GroupByID(aid.String()))
	must(t, e)
	ctid, e := ck.ParseTenantID(tid.String())
	must(t, e)
	scope := ck.Scope{TenantID: ctid, AppID: aid}
	f.tenant = aid.String()
	reader := authtest.NewUser(t, ta)
	f.readerSignIn = func() string { return authtest.SignIn(t, ta, reader).AccessToken }
	policy, e := ckauth.NewAuthorization(t.Context(), ak, ckauth.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: scopeAuth})
	must(t, e)
	scoper, e := ckauth.AppScopes(ak, scope)
	must(t, e)
	c := ck.Config{Scope: &scope, Database: ck.DatabaseConfig{Schema: f.schema}, Content: f.model, ClickHouse: ck.ClickHouseConfig{Addr: os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR"), Username: os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), Password: os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD"), Database: f.db}}
	must(t, ck.MigrateClickHouse(t.Context(), f.pool, c))
	native, e := clickhouse.Open(&clickhouse.Options{Addr: []string{c.ClickHouse.Addr}, Auth: clickhouse.Auth{Database: f.db, Username: c.ClickHouse.Username, Password: c.ClickHouse.Password}, Settings: clickhouse.Settings{"max_threads": 2, "max_insert_threads": 2, "max_memory_usage": 1073741824, "use_query_cache": 0, "log_queries": 1}, ReadTimeout: 5 * time.Minute})
	must(t, e)
	f.ch.Conn = native
	f.ch.phase.Store("setup")
	t.Cleanup(func() { must(t, native.Close()) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.engine, e = ck.New(t.Context(), c, ck.Deps{Postgres: f.pool, ClickHouse: f.ch, Auth: ak.Authenticator(), Authorization: policy, Users: ta.UserInfo(), Scopes: scoper, Clock: func() time.Time { return f.now }, Logger: log})
	must(t, e)
	f.fleet, e = rh.New(t.Context(), f.pool, &river.Config{Schema: f.schema + "_river", Logger: log, Queues: map[string]river.QueueConfig{ck.QueueContentKit: {MaxWorkers: 1}}}, f.engine.RiverJobs())
	must(t, e)
	// Public River control disables unrelated periodic work only in this isolated
	// benchmark fleet. Measured refreshes still use the production worker.
	f.fleet.PeriodicJobs().Clear()
	must(t, f.engine.Start(t.Context(), ck.WithRiverClient(f.fleet)))
	must(t, f.fleet.Start(t.Context()))
	t.Cleanup(func() { must(t, f.fleet.Stop(context.Background())); must(t, f.engine.Close(context.Background())) })
	routes, e := f.engine.Routes(ck.Routes{Prefix: "/content", Groups: f.engine.RouteGroups()})
	must(t, e)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Path, r.Handler)
	}
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	save(t, filepath.Join(out, "metadata.json"), map[string]any{"app": app, "kind": f.kind, "schema": f.schema, "database": f.db, "postgres_database": database, "tenant": f.tenant, "items": f.items, "users": f.users, "days": f.days, "raw_day": f.day, "clock": f.now, "groups": len(scopes), "snapshots": len(scopes) * len(k.Popularity), "base": "d0a9f52060c452537750cc5e55b625f58f95b3af", "go_max_procs": runtime.GOMAXPROCS(0), "edition_shape": "one primary-language edition per work; no translated-edition sensitivity", "limits": "real scoped ContentKit HTTP/River/PG/CH; synthetic catalog; no media bytes, ingress, billing or browser rendering"})
	return f
}
func (f *fixture) seedCatalog() {
	t := f.t
	ctx := t.Context()
	start := time.Now()
	admin := f.engine.Client()
	var input []entities.EntityInput
	for _, slug := range []string{"yaoi", "furry", "futanari", "restricted", "bench"} {
		input = append(input, entities.EntityInput{Kind: "tag", Slug: slug, Names: []entities.Name{{Language: "en", Name: slug, Kind: "name"}}})
	}
	tags, e := admin.PostEntities(ctx, input)
	must(t, e)
	for _, tag := range tags {
		f.tagIDs[tag.Slug] = string(tag.ID)
	}
	attrs := map[string]any{}
	if f.app == "doujins" {
		attrs["format"] = "doujin"
	}
	item, e := admin.PostContents(ctx, catalog.ContentInput{Kind: f.kind, Attributes: attrs, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Attributes: attrs, Text: map[string]catalog.VersionText{"en": {Title: "Synthetic benchmark template", Body: "Synthetic published edition body."}}}}})
	must(t, e)
	_, e = admin.PostContentPublish(ctx, item.ID, catalog.PublishInput{})
	must(t, e)
	deadline := time.Now().Add(time.Minute)
	for {
		var live bool
		must(t, f.pool.QueryRow(ctx, "SELECT released FROM "+f.schema+".content_versions WHERE tenant_id=$1 AND id=$2", f.tenant, item.Versions[0].ID).Scan(&live))
		if live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("template publication did not complete")
		}
		time.Sleep(30 * time.Millisecond)
	}
	item, e = admin.GetContent(ctx, item.ID, ck.GetContentQuery{})
	must(t, e)
	f.template = item
	f.cloneCatalog()
	f.phases = append(f.phases, map[string]any{"phase": "catalog_fixture", "seconds": time.Since(start).Seconds()})
	save(t, filepath.Join(f.out, "phases.json"), f.phases)
}
func (f *fixture) refresh(label string) { f.refreshObserved(label, nil) }
func (f *fixture) refreshObserved(label string, running chan<- struct{}) {
	t := f.t
	ctx := t.Context()
	_, e := f.pool.Exec(ctx, "UPDATE "+f.schema+".content_popular_rankings SET computed_at=$2 WHERE tenant_id=$1", f.tenant, f.now.Add(-2*time.Hour))
	must(t, e)
	f.ch.phase.Store(label)
	start := time.Now()
	job, e := f.fleet.Insert(ctx, refreshArgs{}, &river.InsertOpts{Queue: ck.QueueContentKit, MaxAttempts: 1})
	must(t, e)
	var observedRunning, attempted, completed time.Time
	for {
		j, e := f.fleet.JobGet(ctx, job.Job.ID)
		must(t, e)
		if j.AttemptedAt != nil {
			attempted = *j.AttemptedAt
		}
		if j.State == rivertype.JobStateRunning && observedRunning.IsZero() {
			observedRunning = time.Now()
			if running != nil {
				close(running)
			}
		}
		if j.State == rivertype.JobStateCompleted {
			if j.FinalizedAt != nil {
				completed = *j.FinalizedAt
			}
			break
		}
		if j.State == rivertype.JobStateDiscarded || j.State == rivertype.JobStateRetryable {
			t.Fatalf("actual refresh failed: %+v", j.Errors)
		}
		if time.Since(start) > time.Hour {
			t.Fatal("worker exceeded one hour")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var n int
	must(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM "+f.schema+".content_popular_rankings WHERE tenant_id=$1 AND computed_at IS NOT NULL", f.tenant).Scan(&n))
	want := len(f.model.Kinds[f.kind].Popularity) * len(f.model.Kinds[f.kind].Popularity[0].ActivityScopes())
	if n != want {
		t.Fatalf("got%d snapshots want%d", n, want)
	}
	f.phases = append(f.phases, map[string]any{"phase": label, "seconds": time.Since(start).Seconds(), "job": job.Job.ID, "snapshots": n, "started_at": start, "observed_running_at": observedRunning, "attempted_at": attempted, "completed_at": completed})
	save(t, filepath.Join(f.out, "phases.json"), f.phases)
	f.ch.phase.Store("idle")
	t.Log(label, time.Since(start), n)
}

type refreshArgs struct{}

func (refreshArgs) Kind() string { return "contentkit_popular_refresh" }
