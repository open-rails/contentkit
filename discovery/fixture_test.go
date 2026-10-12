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
			{Group: kinds.GroupRef{Kind: "tag", Slug: "big-breasts"}, Labels: map[string]string{"en": "Big breasts"}},
			{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, DefaultVisible: true},
		},
		Kinds: map[string]kinds.Kind{
			"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Groups: []string{"tag", "artist", "series", "rail"}, Sort: "newest",
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

// existsResolver sees every content item that exists: the catalog's
// resolver stands in until it lands.
type existsResolver struct{ f *fixture }

func (r existsResolver) Resolve(ctx context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		var ok bool
		if err := r.f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+r.f.table("content_items")+` WHERE tenant_id = $1 AND id = $2 AND kind = $3)`,
			ref.TenantID, ref.ContentID, ref.ContentKind).Scan(&ok); err != nil {
			return nil, err
		}
		out[ref.Key()] = access.Resolution{Ref: ref, Visible: ok, Accessible: ok}
	}
	return out, nil
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	schema string
	now    time.Time
	groups *groups.Store
	svc    *Service
	h      http.Handler
	ids    map[string]string // group slug -> id
}

// newFixture serves discovery over a fresh schema; ch is the signal plane
// (nil runs without one).
func newFixture(t *testing.T, ch signal.Conn, chDB string) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	f := &fixture{t: t, ctx: ctx, pool: pool, schema: pgtest.Schema(t, ctx, pool), now: time.Now().UTC().Truncate(time.Second), ids: map[string]string{}}
	cfg := siteConfig()
	var err error
	if f.groups, err = groups.New(groups.Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: cfg}); err != nil {
		t.Fatal(err)
	}
	gate, err := access.NewGate(access.GateConfig{Tenant: tenant, Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(hub.Config{PG: pool, PGSchema: f.schema, Tenant: tenant, DefaultLanguage: "en", CH: ch, CHDatabase: chDB})
	if err != nil {
		t.Fatal(err)
	}
	if f.svc, err = New(Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: cfg, Gate: gate, Groups: f.groups, Hub: h,
		Resolver: existsResolver{f}, Visibility: FacetVisibility(FacetVisibilityOptions{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: cfg, Gate: gate})}); err != nil {
		t.Fatal(err)
	}
	if f.h, err = f.svc.Handler(testauth.Auth{Claims: map[string][]string{"fan": {"premium"}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) table(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }

func vid(n, i int) string { return fmt.Sprintf("01920000-0000-7000-9000-%010d%02d", n, i) }

// group creates groups of kind by slug, with an English name.
func (f *fixture) group(kind string, slugs ...string) {
	f.t.Helper()
	var in []groups.GroupInput
	for _, s := range slugs {
		in = append(in, groups.GroupInput{Kind: kind, Slug: s, Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: strings.ToUpper(s[:1]) + s[1:]}}})
	}
	out, err := f.groups.CreateGroups(f.ctx, in)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, g := range out {
		f.ids[g.Slug] = string(g.ID)
	}
}

// ver is one version as the catalog writes it.
type ver struct {
	langs  []string
	traits []string
	future bool    // not live yet
	rule   *string // access override
	pages  int
}

// item is one content item as the catalog writes it, with its facets.
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
	favorites    int
	popularity   string
	versions     []ver
	groups       []string // slugs, content-level memberships
}

// seed writes items through the contract tables, as the catalog lane will.
func (f *fixture) seed(items ...item) {
	f.t.Helper()
	for _, it := range items {
		if it.kind == "" {
			it.kind = "gallery"
		}
		if it.rule == "" {
			it.rule = `{"level":"public"}`
		}
		if it.listing == "" {
			it.listing = "listed"
		}
		if it.popularity == "" {
			it.popularity = "{}"
		}
		attrs, _ := json.Marshal(it.attrs)
		if it.attrs == nil {
			attrs = []byte("{}")
		}
		state := "published"
		if it.deleted {
			state = "deleted"
		}
		published := f.now.Add(-it.age)
		id := cid(it.n)
		f.exec(`INSERT INTO {s}.content_items (tenant_id, id, kind, state, published_at, listing, access, restrictions, attributes)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, tenant, id, it.kind, state, published, it.listing, it.rule, nonNil(it.restrictions), string(attrs))
		for lang, title := range it.titles {
			f.exec(`INSERT INTO {s}.content_text (tenant_id, content_id, language, title, slug) VALUES ($1, $2, $3, $4, $5)`,
				tenant, id, lang, title, strings.ToLower(strings.ReplaceAll(title, " ", "-")))
		}
		f.exec(`INSERT INTO {s}.content_codes (tenant_id, code, content_kind, content_id, slug) VALUES ($1, $2, $3, $4, $5)`,
			tenant, fmt.Sprintf("G%08d", it.n), it.kind, id, strings.ToLower(strings.ReplaceAll(it.titles["en"], " ", "-")))
		for i, v := range it.versions {
			rule := it.rule
			if v.rule != nil {
				rule = *v.rule
			}
			var r access.Rule
			if err := json.Unmarshal([]byte(rule), &r); err != nil {
				f.t.Fatal(err)
			}
			fc := r.Facet()
			live := &published
			if v.future {
				t := f.now.Add(time.Hour)
				live = &t
			}
			var pages *int
			if v.pages > 0 {
				pages = &v.pages
			}
			f.exec(`INSERT INTO {s}.content_versions (tenant_id, id, content_id, position, languages, traits, state, published_at, access, pages)
 VALUES ($1, $2, $3, $4, $5, $6, 'published', $7, $8, $9)`, tenant, vid(it.n, i), id, i, v.langs, nonNil(v.traits), published, v.rule, pages)
			f.exec(`INSERT INTO {s}.content_facets (tenant_id, content_id, version_id, kind, live_from, deleted, listing, languages, access_level,
 access_tiers, access_group_id, access_free_after, restrictions, traits, position, published_at, attributes, favorites, popularity)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
				tenant, id, vid(it.n, i), it.kind, live, it.deleted, it.listing, v.langs, string(fc.Level), fc.Tiers, fc.GroupID, fc.FreeAfter,
				nonNil(it.restrictions), nonNil(v.traits), i, published, string(attrs), it.favorites, it.popularity)
		}
		var ms []groups.Membership
		for _, slug := range it.groups {
			ms = append(ms, groups.Membership{GroupID: groups.GroupID(f.ids[slug]), ContentID: id})
		}
		if len(ms) > 0 {
			if err := f.groups.AddMemberships(f.ctx, ms); err != nil {
				f.t.Fatal(err)
			}
		}
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
