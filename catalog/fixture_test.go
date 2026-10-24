package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
)

const tenant = "site"

// config is a site with a reader kind (galleries: versions per language,
// traits, segments, attributes), a post kind with one hidden version, tags,
// sellable channels, a premium tier, a restricted flag, three sites and one
// default-hidden content filter.
func config() kinds.Config {
	return kinds.Config{
		Languages:    []string{"en", "ja"},
		Tiers:        []string{"premium"},
		TokenClaims:  []string{"premium"},
		Restrictions: []string{"restricted"}, ContentFilters: kinds.ContentFilters{AppliesTo: []string{"gallery", "blog"}, Domains: []kinds.Domain{{Name: "main", Hosts: []string{"main.test"}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "gore"}}}}, {Name: "gay", Hosts: []string{"gay.test"}, RequireEntities: map[string][]kinds.EntityRef{"gallery": []kinds.EntityRef{{Kind: "tag", Slug: "yaoi"}}, "blog": []kinds.EntityRef{{Kind: "tag", Slug: "yaoi"}}}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "gore"}}}}, {Name: "moe", Hosts: []string{"moe.test"}, Filters: append([]kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "gore"}}}, kinds.ContentFilter{Entity: kinds.EntityRef{Kind: "tag", Slug: "restricted"}, Visible: false, UserToggle: func() *bool { b := false; return &b }()})}}}, Kinds: map[string]kinds.Kind{
			"gallery": {Route: "g", Presentation: "reader", Segments: true, Entities: []string{"tag", "channel", "series"}, PathEntity: "series",
				Versions: kinds.VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Attributes: map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}},
					"volume": {Type: "int"}, "complete": {Type: "bool"}, "released": {Type: "time"}, "note": {Type: "string"}}},
			"blog": {Route: "blog", Presentation: "post", Entities: []string{"tag"}, Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
		},
		EntityKinds: map[string]kinds.EntityKind{
			"tag":     {Route: "tags", VersionLevel: true, Parent: "tag"},
			"channel": {Owner: "user", Entitled: true, AuthScope: true},
			"series":  {Route: "series"},
		},
	}
}

// roles is the test auth: root permissions per actor ("*" holds all) and
// the editors of each entity scope.
type roles struct {
	mu       sync.Mutex
	root     map[string][]string
	entities map[string]map[string]bool
}

func (r *roles) Can(_ context.Context, a access.Actor, perm string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.root[a.ID], "*") || slices.Contains(r.root[a.ID], perm), nil
}

func (r *roles) CanIn(ctx context.Context, a access.Actor, scope auth.Scope, perm string) (bool, error) {
	if scope == testauth.Scope {
		return r.Can(ctx, a, perm)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return scope.Authority == testauth.Issuer && perm == access.EntityContentWrite("channel") && r.entities[scope.ID][a.ID], nil
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	schema   string
	s        *Store
	entities *entities.Store
	roles    *roles
	auth     testauth.Auth // its Claims are each actor's token tier claims
	mu       sync.Mutex
	clock    time.Time
}

// newFixture is a catalog over a fresh schema with the real access Gate
// without billing (tiers come from token claims; held keys are
// adapters/openrails' tests), the real entities store, and a clock the test
// moves.
func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	f := &fixture{t: t, ctx: ctx, pool: pool, schema: pgtest.Schema(t, ctx, pool),
		roles: &roles{root: map[string][]string{"staff": {"*"}, "viewer": {string(access.ContentViewRestricted)}}, entities: map[string]map[string]bool{}},
		clock: time.Now().UTC().Truncate(time.Second)}
	f.auth = testauth.Auth{Authz: f.roles, Claims: map[string][]string{}}
	var err error
	if f.entities, err = entities.New(entities.Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: config(),
		ContentChanged: func(ctx context.Context, q entities.Querier, ids []string) error {
			return f.s.ContentChanged(ctx, q, ids)
		},
		NamesChanged: func(ctx context.Context, q entities.Querier, ids []string) error {
			return f.s.EntitiesRenamed(ctx, q, ids)
		}}); err != nil {
		t.Fatal(err)
	}
	opts := Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: config(), Auth: f.auth,
		Entities: f.entities, Now: f.now}
	if mutate != nil {
		mutate(&opts)
	}
	if f.s, err = New(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.s.Close)
	hasMarker := false
	for _, d := range opts.Kinds.ContentFilters.Domains {
		for _, r := range d.Filters {
			if r.Entity.Kind == "tag" && r.Entity.Slug == "restricted" {
				hasMarker = true
			}
		}
	}
	if hasMarker {
		marker := f.entity("tag", "restricted")
		internal := true
		if _, err := f.entities.UpdateEntities(ctx, []entities.EntityUpdate{{ID: entities.EntityID(marker), Internal: &internal}}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// advance moves the clock.
func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *fixture) table(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }

// entity creates an active entity named in English (channels with an
// AuthKit scope) through the entities module.
func (f *fixture) entity(kind, slug string) string {
	f.t.Helper()
	var existing string
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM `+f.table("content_entities")+` WHERE tenant_id=$1 AND kind=$2 AND slug=$3`, tenant, kind, slug).Scan(&existing); err == nil {
		return existing
	} else if err != pgx.ErrNoRows {
		f.t.Fatal(err)
	}
	in := entities.EntityInput{Kind: kind, Slug: slug, Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: strings.ToUpper(slug[:1]) + slug[1:]}}}
	if kind == "channel" {
		in.AuthScope, in.OwnerUserID = "scope-"+slug, "owner-"+slug
	}
	got, err := f.entities.CreateEntities(f.ctx, []entities.EntityInput{in})
	if err != nil {
		f.t.Fatal(err)
	}
	return string(got[0].ID)
}

// join adds content (or one version) to an entity through the entities module,
// which refreshes its facets and tells the catalog.
func (f *fixture) join(entityID, contentID, versionID string) {
	f.t.Helper()
	if err := f.entities.AddMemberships(f.ctx, []entities.Membership{{EntityID: entities.EntityID(entityID), ContentID: contentID, VersionID: versionID}}); err != nil {
		f.t.Fatal(err)
	}
}

// claim gives actorID's token the tier claims.
func (f *fixture) claim(actorID string, tiers ...string) { f.auth.Claims[actorID] = tiers }

// editor makes actorID an editor of the channel's scope.
func (f *fixture) editor(slug, actorID string) {
	f.roles.mu.Lock()
	defer f.roles.mu.Unlock()
	scope := "scope-" + slug
	if f.roles.entities[scope] == nil {
		f.roles.entities[scope] = map[string]bool{}
	}
	f.roles.entities[scope][actorID] = true
}

// as is the actor as the route gate builds it over the fixture's auth.
func (f *fixture) as(id string) access.Actor {
	if id == "" {
		return access.Actor{Anonymous: true, IP: "10.0.0.1"}
	}
	a := access.Actor{ID: id, Kind: auth.SubjectUser}
	v, err := f.auth.Authenticate(testauth.As(httptest.NewRequest(http.MethodGet, "/", nil), a))
	if err != nil {
		f.t.Fatal(err)
	}
	a.Verified = v
	return a
}

var anon = ""

// create makes one content item as actor, failing the test on error.
func (f *fixture) create(actor string, in ContentInput) string {
	f.t.Helper()
	ids, err := f.s.Create(f.ctx, f.as(actor), []ContentInput{in})
	if err != nil {
		f.t.Fatalf("create %+v: %v", in, err)
	}
	if slices.Contains(in.Restrictions, "restricted") {
		f.join(f.entity("tag", "restricted"), ids[0], "")
	}
	return ids[0]
}

// gallery is a gallery titled title with one version per language.
func gallery(title string, languages ...string) ContentInput {
	in := ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: title}}}
	for _, l := range languages {
		in.Versions = append(in.Versions, VersionInput{Languages: []string{l}})
	}
	return in
}

func (f *fixture) publish(actor, id string, at *time.Time) {
	f.t.Helper()
	if err := f.s.Publish(f.ctx, f.as(actor), []Publish{{ContentID: id, At: at}}); err != nil {
		f.t.Fatalf("publish %s: %v", id, err)
	}
}

// viewer is the request reader for actor on host.
func (f *fixture) viewer(actor, host string, o ViewerOptions) *Viewer {
	f.t.Helper()
	o.Actor, o.Host = f.as(actor), host
	v, err := f.s.Viewer(access.WithMemo(f.ctx), o)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// read is what actor reads of ids on host (nil where nothing).
func (f *fixture) read(actor, host string, ids ...string) []*Content {
	f.t.Helper()
	got, err := f.s.Contents(f.ctx, f.viewer(actor, host, ViewerOptions{}), ids)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// listed is the content ids the viewer's listing shows, in id order.
func (f *fixture) listed(v *Viewer) []string {
	f.t.Helper()
	pred, args := v.Listed("f")
	args["tenant"] = tenant
	rows, err := f.pool.Query(f.ctx, `SELECT DISTINCT f.content_id FROM `+f.table("content_facets")+` f WHERE f.tenant_id = @tenant AND `+pred+` ORDER BY 1`, args)
	if err != nil {
		f.t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return ids
}

// handler mounts the catalog routes at their module's prefix, as a host does.
func (f *fixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(httpapi.Contents.Prefix()+"/", http.StripPrefix(httpapi.Contents.Prefix(), f.s.Handler()))
	return mux
}

// do serves one request as actor (bearer from testauth) on host.
func (f *fixture) do(actor, method, target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	if actor != "" {
		req = testauth.As(req, access.Actor{ID: actor, Kind: "user"})
	}
	req.Host = "main.test"
	rec := httptest.NewRecorder()
	f.handler().ServeHTTP(rec, req)
	return rec
}

// want serves one request and decodes a 2xx body into out.
func (f *fixture) want(status int, actor, method, target string, body, out any) {
	f.t.Helper()
	rec := f.do(actor, method, target, body)
	if rec.Code != status {
		f.t.Fatalf("%s %s as %q: %d, want %d: %s", method, target, actor, rec.Code, status, rec.Body)
	}
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatal(err)
		}
	}
}

func ids(cs []*Content) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		if c != nil {
			out[i] = c.ID
		}
	}
	return out
}

func sorted(ids ...string) []string { return slices.Sorted(slices.Values(ids)) }

func ptr[T any](v T) *T { return &v }
