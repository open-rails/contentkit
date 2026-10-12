package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/access/accesstest"
	"github.com/open-rails/contentkit/contentref"
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
		Restrictions: []string{"restricted"},
		Sites: []kinds.Site{
			{Name: "main", Hosts: []string{"main.test"}},
			{Name: "gay", Hosts: []string{"gay.test"}, RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}},
			{Name: "moe", Hosts: []string{"moe.test"}, ExcludeRestrictions: []string{"restricted"}},
		},
		ContentFilters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "gore"}}},
		Kinds: map[string]kinds.Kind{
			"gallery": {Route: "g", Presentation: "reader", Segments: true, Groups: []string{"tag", "channel"},
				Versions: kinds.VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Attributes: map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}},
					"volume": {Type: "int"}, "complete": {Type: "bool"}, "released": {Type: "time"}, "note": {Type: "string"}}},
			"blog": {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
		},
		GroupKinds: map[string]kinds.GroupKind{
			"tag":     {Route: "tags"},
			"channel": {Owner: "user", Sellable: true, AuthScope: true},
		},
	}
}

// roles is the test auth: root permissions per actor ("*" holds all), and
// the editors of each group scope.
type roles struct {
	mu     sync.Mutex
	root   map[string][]string
	groups map[string]map[string]bool
}

func (r *roles) Can(_ context.Context, a access.Actor, perm string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.root[a.ID], "*") || slices.Contains(r.root[a.ID], perm), nil
}

func (r *roles) CanIn(_ context.Context, a access.Actor, scope, perm string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return perm == EditorPermission && r.groups[scope][a.ID], nil
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	schema string
	s      *Store
	ent    *accesstest.Entitlements
	roles  *roles
	mu     sync.Mutex
	clock  time.Time
}

// newFixture is a catalog over a fresh schema with the real access Gate over
// in-memory entitlements, effective groups read from memberships, and a
// clock the test moves.
func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	f := &fixture{t: t, ctx: ctx, pool: pool, schema: pgtest.Schema(t, ctx, pool), ent: &accesstest.Entitlements{},
		roles: &roles{root: map[string][]string{"staff": {"*"}, "viewer": {string(access.ContentViewRestricted)}}, groups: map[string]map[string]bool{}},
		clock: time.Now().UTC().Truncate(time.Second)}
	gate, err := access.NewGate(access.GateConfig{Entitlements: f.ent, Tenant: tenant, Tiers: []string{"premium"},
		OwnedKinds: []string{"blog", "gallery"}, MemberKinds: []string{"channel"}})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: config(), Auth: testauth.Auth{Authz: f.roles}, Gate: gate,
		Groups: f.effectiveGroups, Now: f.now}
	if mutate != nil {
		mutate(&opts)
	}
	if f.s, err = New(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.s.Close)
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

// effectiveGroups is each version's groups: its content's active memberships
// and its own, of active groups. The groups module owns the real rule.
func (f *fixture) effectiveGroups(ctx context.Context, q Querier, versionIDs []string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `SELECT v.id, m.group_id FROM `+f.table("content_versions")+` v
 JOIN `+f.table("content_memberships")+` m ON m.tenant_id = v.tenant_id AND m.content_id = v.content_id AND (m.version_id IS NULL OR m.version_id = v.id)
 JOIN `+f.table("content_groups")+` g ON g.tenant_id = m.tenant_id AND g.id = m.group_id AND g.state = 'active'
 WHERE v.tenant_id = $1 AND v.id = ANY($2) AND m.state = 'active'`, tenant, versionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var v, g string
		if err := rows.Scan(&v, &g); err != nil {
			return nil, err
		}
		out[v] = append(out[v], g)
	}
	return out, rows.Err()
}

// group creates an active group, named in English, with an AuthKit scope for
// channels.
func (f *fixture) group(kind, slug string) string {
	f.t.Helper()
	id := contentref.NewID()
	var scope *string
	if kind == "channel" {
		s := "scope-" + slug
		scope = &s
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_groups")+` (tenant_id, id, kind, slug, auth_scope) VALUES ($1, $2, $3, $4, $5)`,
		tenant, id, kind, slug, scope); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_group_names")+` (tenant_id, group_id, language, kind, name) VALUES ($1, $2, 'en', 'name', initcap($3))`,
		tenant, id, slug); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// join adds content (or one version) to a group and refreshes its facets in
// the same transaction, as the groups module does.
func (f *fixture) join(groupID, contentID, versionID string) {
	f.t.Helper()
	err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
		var v *string
		if versionID != "" {
			v = &versionID
		}
		if _, err := tx.Exec(f.ctx, `INSERT INTO `+f.table("content_memberships")+` (tenant_id, group_id, content_id, version_id) VALUES ($1, $2, $3, $4)`,
			tenant, groupID, contentID, v); err != nil {
			return err
		}
		return f.s.RefreshFacets(f.ctx, tx, []string{contentID})
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

// editor makes actorID an editor of the channel's scope.
func (f *fixture) editor(slug, actorID string) {
	f.roles.mu.Lock()
	defer f.roles.mu.Unlock()
	scope := "scope-" + slug
	if f.roles.groups[scope] == nil {
		f.roles.groups[scope] = map[string]bool{}
	}
	f.roles.groups[scope][actorID] = true
}

// as is the actor as the route gate builds it over the fixture's auth.
func (f *fixture) as(id string) access.Actor {
	if id == "" {
		return access.Actor{Anonymous: true, IP: "10.0.0.1"}
	}
	return testauth.Actor(access.Actor{ID: id, Kind: "user"}, f.roles)
}

var anon = ""

// create makes one content item as actor, failing the test on error.
func (f *fixture) create(actor string, in ContentInput) string {
	f.t.Helper()
	ids, err := f.s.Create(f.ctx, f.as(actor), []ContentInput{in})
	if err != nil {
		f.t.Fatalf("create %+v: %v", in, err)
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
