package taxonomy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// totalsFixture adds a host work table (hw: access value, tier, listed,
// deleted) beside the version table hv, as both hosts keep access on the work.
func totalsFixture(t *testing.T, ctx context.Context) (*pgxpool.Pool, string) {
	t.Helper()
	pool, schema := testSchema(t, ctx)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.hw (tenant_id text NOT NULL, content_kind text NOT NULL, content_id text NOT NULL,
 access text NOT NULL, tier text, listed boolean NOT NULL DEFAULT true, deleted boolean NOT NULL DEFAULT false, PRIMARY KEY (tenant_id, content_kind, content_id))`, schema)); err != nil {
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

// listedEligibility admits a live version of a listed, undeleted work and
// exposes the work's access columns as h.
func listedEligibility(schema string) search.Eligibility {
	return search.Eligibility{SQL: fmt.Sprintf(`SELECT w.access, w.tier FROM %[1]s.hv hv
 JOIN %[1]s.hw w ON w.tenant_id=hv.tenant_id AND w.content_kind=hv.content_kind AND w.content_id=hv.content_id
 WHERE hv.tenant_id=sd.tenant_id AND hv.content_kind=sd.content_kind AND hv.content_id=sd.content_id
   AND hv.version_id=sd.content_version_id AND hv.language=sd.language AND hv.live AND w.listed AND NOT w.deleted`, schema)}
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

// Totals count each listed work once by who opens it: unlisted, deleted, not
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
		return totalsString(got)
	}
	tagKey := func(slug string) NodeKey { return NodeKey{Kind: "tag", Slug: slug} }
	for _, c := range []struct {
		name string
		q    TotalsQuery
		want string
	}{
		{"every listed kind", TotalsQuery{}, `{"kinds":{"gallery":{"public":1,"tiers":{"premium":3}},"video":{"public":1,"tiers":{"premium":1}}}}`},
		{"one kind", TotalsQuery{Kinds: []string{"gallery"}}, `{"kinds":{"gallery":{"public":1,"tiers":{"premium":3}}}}`},
		{"any of a work tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("yuri")}}}, `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2}}}}`},
		{"none of a work tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{NoneOf: []NodeKey{tagKey("yuri")}}}, `{"kinds":{"gallery":{"public":0,"tiers":{"premium":1}}}}`},
		// colored is effective on g1's live es version and on g6's draft only.
		{"any of a version tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("colored")}}}, `{"kinds":{"gallery":{"public":1,"tiers":{"premium":1}}}}`},
		// g1's en versions lack colored, so g1 still counts.
		{"none of a version tag", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{NoneOf: []NodeKey{tagKey("colored")}}}, `{"kinds":{"gallery":{"public":1,"tiers":{"premium":3}}}}`},
		{"any and none", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("yuri"), tagKey("yuri")}, NoneOf: []NodeKey{tagKey("colored")}}}, `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2}}}}`},
		{"deleted node and proposed assignment are not effective", TotalsQuery{Kinds: []string{"gallery"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("gone"), tagKey("proposed")}}}, `{"kinds":{"gallery":{"public":0,"tiers":{"premium":0}}}}`},
		{"unknown slug", TotalsQuery{Kinds: []string{"video"}, Scope: CatalogScope{AnyOf: []NodeKey{tagKey("nope")}}}, `{"kinds":{"video":{"public":0,"tiers":{"premium":0}}}}`},
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
	if got := totals(b, TotalsQuery{Kinds: []string{"gallery"}}); got != `{"kinds":{"gallery":{"public":2,"tiers":{"premium":2}}}}` {
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
	if got := totals(tiers, TotalsQuery{}); got != `{"kinds":{"gallery":{"public":1,"tiers":{"gold":2,"premium":1}}}}` {
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
		{"main.example", "", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2}},"video":{"public":0,"tiers":{"premium":1}}}}`},
		{"main.example", "?kinds=gallery", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2}}}}`},
		{"main.example", "?kinds=video&kinds=gallery", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2}},"video":{"public":0,"tiers":{"premium":1}}}}`},
		{"yuri.example", "?kinds=gallery,video", 200, "public, max-age=600", `{"kinds":{"gallery":{"public":0,"tiers":{"premium":1}},"video":{"public":0,"tiers":{"premium":0}}}}`},
		{"main.example", "?kinds=tag", 400, "", `{"error":"taxonomy: invalid input: kind \"tag\" is not listed","code":"invalid_request"}`},
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
	got, err := client.Totals(ctx, []string{"video", "gallery"})
	if err != nil || totalsString(got) != `{"kinds":{"gallery":{"public":1,"tiers":{"premium":2}},"video":{"public":0,"tiers":{"premium":1}}}}` {
		t.Fatalf("client totals: %s %v", totalsString(got), err)
	}
	if _, err := client.Totals(ctx, []string{"tag"}); err == nil || !strings.Contains(err.Error(), "400 invalid_request") {
		t.Fatalf("client unknown kind: %v", err)
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
