package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

const tenant = "doujins"

func siteConfig() kinds.Config {
	return kinds.Config{
		Languages:    []string{"en", "ja"},
		Tiers:        []string{"premium"},
		TokenClaims:  []string{"premium"},
		Restrictions: []string{"restricted"},
		Sites: []kinds.Site{
			{Name: "main", Hosts: []string{"main.test"}},
			{Name: "gay", Hosts: []string{"gay.test"}, RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "yaoi"}}},
			{Name: "clean", Hosts: []string{"clean.test"}, ExcludeRestrictions: []string{"restricted"}},
		},
		ContentFilters: []kinds.ContentFilter{
			{Group: kinds.GroupRef{Kind: "tag", Slug: "big-breasts"}},
			{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, DefaultVisible: true},
		},
		Kinds: map[string]kinds.Kind{
			"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Groups: []string{"tag", "artist", "series", "rail"}, Sort: "newest",
				Locked:     &kinds.Locked{Show: kinds.Shown, Preview: &kinds.Preview{Pages: 2}},
				Attributes: map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}, Filterable: true}}},
			"video": {Route: "watch", Presentation: "player", Versions: kinds.VersionRules{Languages: "many"}, Groups: []string{"tag"}},
		},
		GroupKinds: map[string]kinds.GroupKind{
			"tag":    {Route: "tags"},
			"artist": {Route: "artists"},
			"series": {Route: "series", Ordered: true},
			"rail":   {Ordered: true, Windows: true},
		},
	}
}

// staffOnly holds every permission for the actor "staff": the catalog's
// writer in the seeds.
type staffOnly struct{}

func (staffOnly) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff", nil
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	schema string
	now    time.Time
	clock  time.Time // the catalog's; the seeds publish in the past
	groups *groups.Store
	cat    *catalog.Store
	svc    *Service
	h      http.Handler
	ids    map[string]string // group slug -> id
	cids   map[int]string    // item n -> content id
	vids   map[[2]int]string // item n, version i -> version id
	codes  map[int]string    // item n -> content code
}

// newFixture serves discovery over a fresh schema; ch is the signal plane
// (nil runs without one).
func newFixture(t *testing.T, ch signal.Conn, chDB string) *fixture {
	return newFixtureConfig(t, ch, chDB, siteConfig())
}

func newFixtureConfig(t *testing.T, ch signal.Conn, chDB string, cfg kinds.Config) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	f := &fixture{t: t, ctx: ctx, pool: pool, schema: pgtest.Schema(t, ctx, pool), now: time.Now().UTC().Truncate(time.Second), ids: map[string]string{},
		cids: map[int]string{}, vids: map[[2]int]string{}, codes: map[int]string{}}
	f.clock = f.now
	var err error
	if f.groups, err = groups.New(groups.Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: cfg,
		ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
			return f.cat.ContentChanged(ctx, q, ids)
		}}); err != nil {
		t.Fatal(err)
	}
	gate, err := access.NewGate(access.GateConfig{Tenant: tenant, Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.cat, err = catalog.New(ctx, catalog.Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: cfg, Auth: testauth.Auth{Authz: staffOnly{}},
		Gate: gate, Groups: f.groups, Now: func() time.Time { return f.clock }}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.cat.Close)
	h, err := hub.New(hub.Config{PG: pool, PGSchema: f.schema, Tenant: tenant, DefaultLanguage: "en", CH: ch, CHDatabase: chDB})
	if err != nil {
		t.Fatal(err)
	}
	if f.svc, err = New(Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: cfg, Gate: gate, Groups: f.groups, Hub: h,
		Resolver: f.cat.Resolver(), Visibility: f.cat}); err != nil {
		t.Fatal(err)
	}
	if f.h, err = f.svc.Handler(testauth.Auth{Authz: staffOnly{}, Claims: map[string][]string{"fan": {"premium"}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) table(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }

// cid is seeded item n's content id; an unseeded n names no content.
func (f *fixture) cid(n int) string {
	if id, ok := f.cids[n]; ok {
		return id
	}
	return cid(n)
}

// vid is seeded item n's version i.
func (f *fixture) vid(n, i int) string {
	if id, ok := f.vids[[2]int{n, i}]; ok {
		return id
	}
	return fmt.Sprintf("01920000-0000-7000-9000-%010d%02d", n, i)
}

// group creates groups of kind by slug, with an English name.
func (f *fixture) group(kind string, slugs ...string) {
	f.t.Helper()
	var in []groups.GroupInput
	for _, s := range slugs {
		in = append(in, groups.GroupInput{Kind: kind, Slug: s, Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: strings.ToUpper(s[:1]) + strings.ReplaceAll(s[1:], "-", " ")}}})
	}
	out, err := f.groups.CreateGroups(f.ctx, in)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, g := range out {
		f.ids[g.Slug] = string(g.ID)
	}
}

// ver is one version of a seeded item.
type ver struct {
	langs  []string
	traits []string
	future bool    // not live yet
	rule   *string // access override
	pages  int     // media's fact
}

// item is one content item to seed.
type item struct {
	n            int
	kind         string
	titles       map[string]string
	age          time.Duration // published this long ago
	rule         string        // access rule JSON; public by default
	listing      string
	restrictions []string
	attrs        map[string]any
	deleted      bool
	favorites    int    // the counts job's
	popularity   string // discovery's popularity job's
	viewBaseline int64  // an import's legacy views
	versions     []ver
	groups       []string // slugs, content-level memberships
}

// seed creates items through the catalog as staff, publishing each age ago.
// Pages, favorites and popularity are written as the jobs that own them do.
func (f *fixture) seed(items ...item) {
	f.t.Helper()
	staff := testauth.Actor(access.Actor{ID: "staff"}, staffOnly{})
	defer func() { f.clock = f.now }()
	rule := func(raw string) *access.Rule {
		var r access.Rule
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			f.t.Fatal(err)
		}
		return &r
	}
	for _, it := range items {
		if it.kind == "" {
			it.kind = "gallery"
		}
		if it.rule == "" {
			it.rule = `{"level":"public"}`
		}
		in := catalog.ContentInput{Kind: it.kind, Text: map[string]catalog.Text{}, Listing: it.listing, Access: rule(it.rule),
			Restrictions: it.restrictions, Attributes: it.attrs}
		for lang, title := range it.titles {
			in.Text[lang] = catalog.Text{Title: title}
		}
		for _, v := range it.versions {
			vi := catalog.VersionInput{Languages: v.langs, Traits: &v.traits}
			if v.rule != nil {
				vi.Access = rule(*v.rule)
			}
			if v.future {
				at := f.now.Add(time.Hour)
				vi.PublishAt = &at
			}
			in.Versions = append(in.Versions, vi)
		}
		f.clock = f.now.Add(-it.age)
		ids, err := f.cat.Create(f.ctx, staff, []catalog.ContentInput{in})
		if err != nil {
			f.t.Fatalf("seed %d: %v", it.n, err)
		}
		id := ids[0]
		if err := f.cat.Publish(f.ctx, staff, []catalog.Publish{{ContentID: id}}); err != nil {
			f.t.Fatal(err)
		}
		f.cids[it.n] = id
		rows, err := f.pool.Query(f.ctx, `SELECT id FROM `+f.table("content_versions")+` WHERE tenant_id = $1 AND content_id = $2 ORDER BY position`, tenant, id)
		if err != nil {
			f.t.Fatal(err)
		}
		vs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			f.t.Fatal(err)
		}
		for i, v := range it.versions {
			f.vids[[2]int{it.n, i}] = vs[i]
			if v.pages > 0 {
				f.exec(`UPDATE {s}.content_versions SET pages = $2 WHERE id = $1`, vs[i], v.pages)
			}
		}
		var code string
		if err := f.pool.QueryRow(f.ctx, `SELECT code FROM `+f.table("content_codes")+` WHERE tenant_id = $1 AND content_id = $2`, tenant, id).Scan(&code); err != nil {
			f.t.Fatal(err)
		}
		f.codes[it.n] = code
		var ms []groups.Membership
		for _, slug := range it.groups {
			ms = append(ms, groups.Membership{GroupID: groups.GroupID(f.ids[slug]), ContentID: id})
		}
		if len(ms) > 0 {
			if err := f.groups.AddMemberships(f.ctx, ms); err != nil {
				f.t.Fatal(err)
			}
		}
		if it.deleted {
			if err := f.cat.Delete(f.ctx, staff, []string{id}); err != nil {
				f.t.Fatal(err)
			}
		}
		if it.popularity == "" {
			it.popularity = "{}"
		}
		f.exec(`UPDATE {s}.content_items SET view_baseline = $2 WHERE id = $1`, id, it.viewBaseline)
		f.exec(`UPDATE {s}.content_facets SET favorites = $2, popularity = $3, view_baseline = $4 WHERE content_id = $1`, id, it.favorites, it.popularity, it.viewBaseline)
	}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, strings.ReplaceAll(sql, "{s}", pgx.Identifier{f.schema}.Sanitize()), args...); err != nil {
		f.t.Fatal(err)
	}
}

// docs indexes keyword documents, as the search worker does.
func (f *fixture) docs(docs ...search.KeywordDocument) {
	f.t.Helper()
	if err := search.UpsertKeywordDocuments(f.ctx, f.pool, f.schema, docs); err != nil {
		f.t.Fatal(err)
	}
}

func doc(kind, id, lang, title string) search.KeywordDocument {
	return search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: contentref.New(tenant, kind, id), Language: lang}, Title: title}
}

// call sends one request as user ("" anonymous) to host and decodes a 2xx
// body into out; it returns the status and the error code.
func (f *fixture) call(method, host, user, path string, body, out any) (int, string) {
	f.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "http://"+host+path, rd)
	r.Host = host
	if user != "" {
		r = testauth.As(r, access.Actor{ID: user})
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code >= 300 {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		return w.Code, e.Code
	}
	if out != nil && w.Code != http.StatusNoContent {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			f.t.Fatalf("%s %s: %v: %s", method, path, err, w.Body.String())
		}
	}
	return w.Code, ""
}

// list GETs path as user on host and returns its cards' content ids.
func (f *fixture) list(host, user, path string) ([]string, ContentPage) {
	f.t.Helper()
	var page ContentPage
	if status, code := f.call(http.MethodGet, host, user, path, nil, &page); status != http.StatusOK {
		f.t.Fatalf("GET %s: %d %s", path, status, code)
	}
	ids := []string{}
	for _, c := range page.Contents {
		ids = append(ids, c.ID)
	}
	return ids, page
}

func q(path string, kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return path + "?" + v.Encode()
}
