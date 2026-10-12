package taxonomy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/cache"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// totalsFixture adds a host work table (hw: access value, tier, listed,
// deleted, shown in the anonymous listing) beside the version table hv, as
// both hosts keep access on the work, and the further languages a version
// supports (hvl), as one hentai0 version serves several.
func totalsFixture(t *testing.T, ctx context.Context) (*pgxpool.Pool, string) {
	t.Helper()
	pool, schema := testSchema(t, ctx)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %[1]s.hw (tenant_id text NOT NULL, content_kind text NOT NULL, content_id text NOT NULL,
 access text NOT NULL, tier text, listed boolean NOT NULL DEFAULT true, deleted boolean NOT NULL DEFAULT false,
 public_listed boolean NOT NULL DEFAULT true, PRIMARY KEY (tenant_id, content_kind, content_id));
 CREATE TABLE %[1]s.hvl (tenant_id text NOT NULL, content_kind text NOT NULL, version_id text NOT NULL, language text NOT NULL)`, schema)); err != nil {
		t.Fatal(err)
	}
	return pool, schema
}

type hostWork struct {
	kind, id, access, tier string
	listed, deleted        bool
}

func addWorks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, tenantID string, works ...hostWork) {
	t.Helper()
	for _, w := range works {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.hw VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7)`, schema),
			tenantID, w.kind, w.id, w.access, w.tier, w.listed, w.deleted); err != nil {
			t.Fatal(err)
		}
	}
}

// listedEligibility admits a live version of a listed, undeleted work in a
// language it supports and exposes the work's access columns as h.
func listedEligibility(schema string) search.Eligibility {
	return search.Eligibility{SQL: fmt.Sprintf(`SELECT w.access, w.tier, w.public_listed FROM %[1]s.hv hv
 JOIN %[1]s.hw w ON w.tenant_id=hv.tenant_id AND w.content_kind=hv.content_kind AND w.content_id=hv.content_id
 WHERE hv.tenant_id=sd.tenant_id AND hv.content_kind=sd.content_kind AND hv.content_id=sd.content_id
   AND hv.version_id=sd.content_version_id AND hv.live AND w.listed AND NOT w.deleted
   AND (hv.language=sd.language OR EXISTS (SELECT 1 FROM %[1]s.hvl x
     WHERE x.tenant_id=hv.tenant_id AND x.content_kind=hv.content_kind AND x.version_id=hv.version_id AND x.language=sd.language))`, schema)}
}

func premiumListing(schema string) *Listing {
	return &Listing{Kinds: []string{"gallery", "video"}, Eligibility: listedEligibility(schema),
		Access: access.Columns{Level: "h.access", TierName: "premium",
			Levels: map[string]access.Level{"free": access.Public, "premium": access.TierLevel, "ppv": access.PPV}}}
}

func totalsString(t Totals) string {
	b, _ := json.Marshal(t)
	return string(b)
}

// brief renders totals as "kind public/tier… lang:public/tier… | …", tiers and
// languages sorted.
func brief(t Totals) string {
	counts := func(public int64, tiers map[string]int64) string {
		out := fmt.Sprint(public)
		for _, k := range slices.Sorted(maps.Keys(tiers)) {
			out += "/" + fmt.Sprint(tiers[k])
		}
		return out
	}
	var kinds []string
	for _, k := range slices.Sorted(maps.Keys(t.Kinds)) {
		total := t.Kinds[k]
		line := k + " " + counts(total.Public, total.Tiers)
		for _, l := range slices.Sorted(maps.Keys(total.Languages)) {
			line += " " + l + ":" + counts(total.Languages[l].Public, total.Languages[l].Tiers)
		}
		kinds = append(kinds, line)
	}
	return strings.Join(kinds, " | ")
}

// Totals count each listed work by who opens it, once per language it is
// available in and once per kind, never per version: unlisted, deleted, not
// live, paid-per-item, unmapped and document-less works never count; a site
// scope narrows by the nodes effective on the counted version; tenants never
// see each other's works.
func TestCatalogTotalsIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := totalsFixture(t, ctx)
	const other = "hentai0"
	addWorks(t, ctx, pool, schema, tenant,
		hostWork{"gallery", cid(1), "free", "", true, false},
		hostWork{"gallery", cid(2), "premium", "", true, false},
		hostWork{"gallery", cid(3), "free", "", false, false}, // unlisted
		hostWork{"gallery", cid(4), "free", "", true, true},   // deleted
		hostWork{"gallery", cid(5), "free", "", true, false},  // never live
		hostWork{"gallery", cid(6), "premium", "", true, false},
		hostWork{"gallery", cid(7), "ppv", "", true, false},
		hostWork{"gallery", cid(8), "free", "", true, false}, // no search document
		hostWork{"gallery", cid(9), "weird", "", true, false},
		hostWork{"video", cid(11), "free", "", true, false},
	)
	addWorks(t, ctx, pool, schema, other, hostWork{"gallery", cid(1), "free", "", true, false}, hostWork{"gallery", cid(21), "free", "", true, false})
	indexVersions(t, ctx, pool, schema, tenant,
		version{"gallery", cid(1), "g1en", "en", "Blue", true, true},
		version{"gallery", cid(1), "g1es", "es", "Azul", true, true},
		version{"gallery", cid(1), "g1en2", "en", "Blue II", true, false},
		version{"gallery", cid(2), "g2", "en", "Red", true, true},
		version{"gallery", cid(3), "g3", "en", "Hidden", true, true},
		version{"gallery", cid(4), "g4", "en", "Gone", true, true},
		version{"gallery", cid(5), "g5", "en", "Soon", false, true},
		version{"gallery", cid(6), "g6live", "en", "Green", true, true},
		version{"gallery", cid(6), "g6draft", "ja", "Midori", false, false},
		version{"gallery", cid(7), "g7", "en", "Bought", true, true},
		version{"gallery", cid(9), "g9", "en", "Odd", true, true},
		version{"video", cid(11), "m1", "en", "Movie", true, true},
	)
	// One video version serves en and ja; its es document is not a language it supports.
	if err := search.UpsertKeywordDocuments(ctx, pool, schema, []search.KeywordDocument{
		{DocumentKey: search.DocumentKey{ContentRef: contentref.NewVersion(tenant, "video", cid(11), "m1"), Language: "ja"}, Title: "Eiga"},
		{DocumentKey: search.DocumentKey{ContentRef: contentref.NewVersion(tenant, "video", cid(11), "m1"), Language: "es"}, Title: "Película"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.hvl VALUES ($1,'video','m1','ja')`, schema), tenant); err != nil {
		t.Fatal(err)
	}
	indexVersions(t, ctx, pool, schema, other,
		version{"gallery", cid(1), "o1", "en", "Theirs", true, true},
		version{"gallery", cid(21), "o21", "en", "Theirs too", true, true},
	)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.hv VALUES ($1,'gallery',$2,'g8','en',true,true)`, schema), tenant, cid(8)); err != nil {
		t.Fatal(err)
	}

	a := newStore(t, pool, schema, tenant, func(o *Options) { o.Listing = premiumListing(schema) })
	mustCreate(t, ctx, a, tag("tag", "yuri"), tag("colored", "colored"), tag("absent", "gone"), tag("t1", "proposed"))
	g := func(n int) contentref.ContentRef { return work(tenant, "gallery", cid(n)) }
	if err := a.Assign(ctx, []Assignment{
		assign(g(1), "tag", ""), assign(g(2), "tag", ""),
		assign(g(1).WithVersion("g1es"), "colored", ""), assign(g(6).WithVersion("g6draft"), "colored", ""),
		assign(g(6), "absent", ""), {ContentRef: g(6), TaxonomyID: tid("t1"), State: AssignmentProposed},
	}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateNode(ctx, tid("absent"), NodeUpdate{State: ptr(StateDeleted)}); err != nil {
		t.Fatal(err)
	}

	totals := func(s *Store, q TotalsQuery) string {
		t.Helper()
		got, err := s.Totals(ctx, q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		return brief(got)
	}
	tagKey := func(slug string) NodeKey { return NodeKey{Kind: "tag", Slug: slug} }
	for _, c := range []struct {
		name string
		q    TotalsQuery
		want string
	}{
		// g1 has two en versions and one es version: once in en, once in es, once in all.
		{"every listed kind", TotalsQuery{}, "gallery 1/3 en:1/3 es:1/1 | video 1/1 en:1/1 ja:1/1"},
		{"one kind", TotalsQuery{Kinds: []string{"gallery"}}, "gallery 1/3 en:1/3 es:1/1"},
		{"some languages", TotalsQuery{Kinds: []string{"gallery", "video"}, Languages: []string{"ES", "fr", "ja", "es"}}, "gallery 1/3 es:1/1 fr:0/0 ja:0/0 | video 1/1 es:0/0 fr:0/0 ja:1/1"},
		{"any of a work tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("yuri")}}}, "gallery 1/2 en:1/2 es:1/1"},
		{"none of a work tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{NoneOf: []NodeKey{tagKey("yuri")}}}, "gallery 0/1 en:0/1"},
		// colored is effective on g1's live es version and on g6's draft only.
		{"any of a version tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("colored")}}}, "gallery 1/1 es:1/1"},
		// g1's en versions lack colored, so g1 still counts, in en only.
		{"none of a version tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{NoneOf: []NodeKey{tagKey("colored")}}}, "gallery 1/3 en:1/3"},
		{"any and none", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("yuri"), tagKey("yuri")}, NoneOf: []NodeKey{tagKey("colored")}}}, "gallery 1/2 en:1/2"},
		{"deleted node and proposed assignment are not effective", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("gone"), tagKey("proposed")}}}, "gallery 0/0"},
		{"unknown slug", TotalsQuery{Kinds: []string{"video"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("nope")}}}, "video 0/0"},
	} {
		if got := totals(a, c.q); got != c.want {
			t.Errorf("%s: %s\n want %s", c.name, got, c.want)
		}
	}

	for _, q := range []TotalsQuery{
		{Kinds: []string{"tag"}},
		{Kinds: []string{"post"}},
		{Scope: CatalogScope{AnyOf: []NodeKey{{Kind: "gallery", Slug: "x"}}}},
		{Scope: CatalogScope{NoneOf: []NodeKey{{Kind: "tag", Slug: "a b"}}}},
		{Languages: []string{"e n"}},
	} {
		if _, err := a.Totals(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", q, err)
		}
	}
	if _, err := newStore(t, pool, schema, tenant, nil).Totals(ctx, TotalsQuery{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("without a Listing: %v", err)
	}

	// The other tenant counts only its own works, though it shares a content id.
	b := newStore(t, pool, schema, other, func(o *Options) { o.Listing = premiumListing(schema) })
	if got := totals(b, TotalsQuery{Kinds: []string{"gallery"}}); got != "gallery 2/2 en:2/2" {
		t.Errorf("other tenant: %s", got)
	}

	// A per-row tier: each declared tier opens its own rows and every public one.
	tiers := newStore(t, pool, schema, tenant, func(o *Options) {
		o.Listing = &Listing{Kinds: []string{"gallery"}, Eligibility: listedEligibility(schema), Tiers: []string{"gold", "premium"},
			Access: access.Columns{Level: "h.access", Tier: "h.tier", ArgPrefix: "acc_",
				Levels: map[string]access.Level{"free": access.Public, "premium": access.TierLevel}}}
	})
	for id, tier := range map[int]string{2: "gold", 6: "undeclared"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.hw SET tier=$3 WHERE tenant_id=$1 AND content_id=$2`, schema), tenant, cid(id), tier); err != nil {
			t.Fatal(err)
		}
	}
	if got := totals(tiers, TotalsQuery{}); got != "gallery 1/2/1 en:1/2/1 es:1/1/1" {
		t.Errorf("per-row tiers: %s", got)
	}

	for name, l := range map[string]*Listing{
		"no kinds":         {Eligibility: listedEligibility(schema), Access: premiumListing(schema).Access},
		"taxonomy kind":    {Kinds: []string{"tag"}, Eligibility: listedEligibility(schema), Access: premiumListing(schema).Access},
		"no sql":           {Kinds: []string{"gallery"}, Access: premiumListing(schema).Access},
		"no levels":        {Kinds: []string{"gallery"}, Eligibility: listedEligibility(schema), Access: access.Columns{Level: "h.access"}},
		"tier not covered": {Kinds: []string{"gallery"}, Eligibility: listedEligibility(schema), Tiers: []string{"gold"}, Access: premiumListing(schema).Access},
		"reserved arg":     {Kinds: []string{"gallery"}, Eligibility: search.Eligibility{SQL: "SELECT 'free' AS access", Args: map[string]any{"kinds": 1}}, Access: premiumListing(schema).Access},
	} {
		if _, err := New(Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: []string{"tag"}, Languages: []string{"en"}, Listing: l}); !errors.Is(err, ErrInvalid) {
			t.Errorf("listing %s: %v, want ErrInvalid", name, err)
		}
	}

	// The anonymous listing hides g2 and g12; members still see both, so only
	// public drops them: a free work counts for public only where it is listed.
	addWorks(t, ctx, pool, schema, tenant, hostWork{"gallery", cid(12), "free", "", true, false})
	indexVersions(t, ctx, pool, schema, tenant, version{"gallery", cid(12), "g12", "en", "Members only", true, true})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.hw SET public_listed=false WHERE tenant_id=$1 AND content_id=ANY($2)`, schema), tenant, []string{cid(2), cid(12)}); err != nil {
		t.Fatal(err)
	}
	split := newStore(t, pool, schema, tenant, func(o *Options) {
		o.Listing = premiumListing(schema)
		o.Listing.PublicListed = "h.public_listed"
	})
	if got := totals(split, TotalsQuery{Kinds: []string{"gallery"}}); got != "gallery 1/4 en:1/4 es:1/1" {
		t.Errorf("public listing: %s", got)
	}
	if got := totals(split, TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("yuri")}}}); got != "gallery 1/2 en:1/2 es:1/1" {
		t.Errorf("public listing, scoped: %s", got)
	}
}

// The public route answers the listing's totals in the scope the host derives
// from the request, cacheable for ten minutes; a sibling reads it with
// TotalsClient over real HTTP.
func TestCatalogHandlerIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := totalsFixture(t, ctx)
	addWorks(t, ctx, pool, schema, tenant,
		hostWork{"gallery", cid(1), "free", "", true, false},
		hostWork{"gallery", cid(2), "premium", "", true, false},
		hostWork{"video", cid(11), "premium", "", true, false},
	)
	indexVersions(t, ctx, pool, schema, tenant,
		version{"gallery", cid(1), "g1", "en", "Blue", true, true},
		version{"gallery", cid(2), "g2", "en", "Red", true, true},
		version{"video", cid(11), "m1", "en", "Movie", true, true},
	)
	s := newStore(t, pool, schema, tenant, func(o *Options) { o.Listing = premiumListing(schema) })
	mustCreate(t, ctx, s, tag("tag", "yuri"))
	if err := s.Assign(ctx, []Assignment{assign(work(tenant, "gallery", cid(2)), "tag", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	h, err := s.CatalogHandler(CatalogOptions{Scope: func(r *http.Request) (CatalogScope, error) {
		switch r.Host {
		case "yuri.example":
			return CatalogScope{AnyOf: []NodeKey{{Kind: "tag", Slug: "yuri"}}}, nil
		case "broken.example":
			return CatalogScope{AnyOf: []NodeKey{{Kind: "nope", Slug: "x"}}}, nil
		case "down.example":
			return CatalogScope{}, errors.New("flavor store down")
		}
		return CatalogScope{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.StripPrefix("/api/v1/contentkit/catalog", h))
	t.Cleanup(srv.Close)
	get := func(host, query string) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/contentkit/catalog/totals"+query, nil)
		req.Host = host
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Cache-Control"), strings.TrimSpace(string(body))
	}
	for _, c := range []struct {
		host, query string
		status      int
		cache, body string
	}{
		{"main.example", "", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2},"languages":{"en":{"public":1,"tiers":{"premium":2}}}},"video":{"public":0,"tiers":{"premium":1},"languages":{"en":{"public":0,"tiers":{"premium":1}}}}}}`},
		{"main.example", "?kinds=gallery", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2},"languages":{"en":{"public":1,"tiers":{"premium":2}}}}}}`},
		{"main.example", "?kinds=video&kinds=gallery&languages=es", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2},"languages":{"es":{"public":0,"tiers":{"premium":0}}}},"video":{"public":0,"tiers":{"premium":1},"languages":{"es":{"public":0,"tiers":{"premium":0}}}}}}`},
		{"yuri.example", "?kinds=gallery,video&languages=en", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":0,"tiers":{"premium":1},"languages":{"en":{"public":0,"tiers":{"premium":1}}}},"video":{"public":0,"tiers":{"premium":0},"languages":{"en":{"public":0,"tiers":{"premium":0}}}}}}`},
		{"main.example", "?kinds=tag", 400, "", `{"error":"taxonomy: invalid input: kind \"tag\" is not listed","code":"invalid_request"}`},
		{"main.example", "?languages=e%20n", 400, "", `{"error":"taxonomy: invalid input: language \"e n\"","code":"invalid_request"}`},
		{"broken.example", "", 500, "", `{"error":"internal error","code":"internal_error"}`},
		{"down.example", "", 500, "", `{"error":"internal error","code":"internal_error"}`},
	} {
		status, cache, body := get(c.host, c.query)
		if status != c.status || cache != c.cache || body != c.body {
			t.Errorf("%s %s: %d %q %s\n want %d %q %s", c.host, c.query, status, cache, body, c.status, c.cache, c.body)
		}
	}

	client, err := NewTotalsClient(srv.URL+"/api/v1/contentkit/", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Totals(ctx, TotalsQuery{Kinds: []string{"video", "gallery"}})
	if err != nil || brief(got) != "gallery 1/2 en:1/2 | video 0/1 en:0/1" {
		t.Fatalf("client totals: %s %v", brief(got), err)
	}
	if got, err = client.Totals(ctx, TotalsQuery{Kinds: []string{"gallery"}, Languages: []string{"en", "ja"}}); err != nil || brief(got) != "gallery 1/2 en:1/2 ja:0/0" {
		t.Fatalf("client totals by language: %s %v", brief(got), err)
	}
	if _, err := client.Totals(ctx, TotalsQuery{Kinds: []string{"tag"}}); err == nil || !strings.Contains(err.Error(), "400 invalid_request") {
		t.Fatalf("client unknown kind: %v", err)
	}
	for _, q := range []TotalsQuery{{}, {Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{{Kind: "tag", Slug: "yuri"}}}}} {
		if _, err := client.Totals(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Fatalf("client %+v: %v", q, err)
		}
	}
	if _, err := NewTotalsClient("/relative", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative base: %v", err)
	}

	// A count that fails with nothing to fall back on is 503 unavailable.
	broken := newStore(t, pool, schema, tenant, func(o *Options) {
		o.Listing = premiumListing(schema)
		o.Listing.Eligibility.SQL = "SELECT w.access FROM " + schema + ".missing w"
	})
	bh, err := broken.CatalogHandler(CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	bh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/totals", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "" ||
		strings.TrimSpace(rec.Body.String()) != `{"error":"catalog totals are unavailable","code":"unavailable"}` {
		t.Fatalf("broken: %d %q %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	if _, err := newStore(t, pool, schema, tenant, nil).CatalogHandler(CatalogOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("handler without a Listing: %v", err)
	}
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func loadFailures(name string) uint64 {
	for _, s := range cache.Snapshot() {
		if s.Name == name {
			return s.LoadFailures
		}
	}
	return 0
}

// eventually polls f for up to five seconds: background reloads land on their own.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !f(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
	}
}

// Totals serve the cached count for ten minutes, then reload in the
// background; while reloads fail the last good count serves for a day, after
// which the route answers 503 until counting works again. TotalsClient keeps a
// sibling's totals the same way.
func TestCatalogTotalsCacheIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := totalsFixture(t, ctx)
	addWorks(t, ctx, pool, schema, tenant, hostWork{"gallery", cid(1), "free", "", true, false}, hostWork{"gallery", cid(2), "free", "", true, false})
	indexVersions(t, ctx, pool, schema, tenant, version{"gallery", cid(1), "g1", "en", "Blue", true, true})
	s := newStore(t, pool, schema, tenant, func(o *Options) { o.Listing = premiumListing(schema) })
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	s.Close()
	policy := totalsPolicy
	policy.Clock = clk
	var err error
	if s.totals, err = cache.New(policy, s.loadTotals); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	public := func() (int64, error) {
		got, err := s.Totals(ctx, TotalsQuery{Kinds: []string{"gallery"}})
		return got.Kinds["gallery"].Public, err
	}
	if n, err := public(); n != 1 || err != nil {
		t.Fatalf("first count %d %v", n, err)
	}
	indexVersions(t, ctx, pool, schema, tenant, version{"gallery", cid(2), "g2", "en", "Red", true, true})
	clk.advance(9 * time.Minute)
	if n, err := public(); n != 1 || err != nil {
		t.Fatalf("within ten minutes %d %v", n, err)
	}
	clk.advance(2 * time.Minute)
	eventually(t, "the background reload counts g2", func() bool { n, err := public(); return n == 2 && err == nil })

	// Counting breaks: the last good count serves.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %[1]s.hw RENAME TO hw_gone`, schema)); err != nil {
		t.Fatal(err)
	}
	failed := loadFailures(policy.Name)
	clk.advance(11 * time.Minute)
	if n, err := public(); n != 2 || err != nil {
		t.Fatalf("stale %d %v", n, err)
	}
	eventually(t, "the background reload fails", func() bool { return loadFailures(policy.Name) > failed })
	if n, err := public(); n != 2 || err != nil {
		t.Fatalf("after a failed reload %d %v", n, err)
	}
	h, err := s.CatalogHandler(CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(24 * time.Hour)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/totals", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a day stale: %d %s", rec.Code, rec.Body)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %[1]s.hw_gone RENAME TO hw`, schema)); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute) // past the failure backoff
	if n, err := public(); n != 2 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}

	// The client: one request per refresh, the last good totals while the sibling is down.
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.StripPrefix("/ck/catalog", h).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := NewTotalsClient(srv.URL+"/ck", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	remote := remoteTotalsPolicy
	remote.Clock = clk
	if client.totals, err = cache.New(remote, client.load); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	for range 3 {
		if got, err := client.Totals(ctx, TotalsQuery{Kinds: []string{"gallery"}}); err != nil || got.Kinds["gallery"].Public != 2 {
			t.Fatalf("client %+v %v", got, err)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("client sent %d requests, want 1", n)
	}
	srv.Close()
	clientFailed := loadFailures(remote.Name)
	clk.advance(11 * time.Minute)
	if got, err := client.Totals(ctx, TotalsQuery{Kinds: []string{"gallery"}}); err != nil || got.Kinds["gallery"].Public != 2 {
		t.Fatalf("client stale %+v %v", got, err)
	}
	eventually(t, "the client's background reload fails", func() bool { return loadFailures(remote.Name) > clientFailed })
	if got, err := client.Totals(ctx, TotalsQuery{Kinds: []string{"gallery"}}); err != nil || got.Kinds["gallery"].Public != 2 {
		t.Fatalf("client after a failed reload %+v %v", got, err)
	}
}
