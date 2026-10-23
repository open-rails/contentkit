package access_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
)

// AudiencesSQL names, per content_facets row, exactly the audiences that open
// it without buying: what Decide answers for an anonymous viewer and for a
// viewer whose token claims one tier.
func TestAudiencesMatchDecide(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	rules := []access.Rule{
		{Level: access.Public},
		{Level: access.TierLevel, Tiers: []string{"premium"}},
		{Level: access.TierLevel, Tiers: []string{"gold", "premium"}},
		{Level: access.GroupLevel, Group: uid(900)},
		{Level: access.PPV},
		{Level: access.GroupPPV, Group: uid(900)},
		{Level: access.PPV, Releases: []access.Release{{At: past, Level: access.Public}}},
		{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: future, Level: access.Public}}},
	}
	var items []access.Item
	for i, r := range rules {
		c, v := uid(i+1), uid(100+i)
		f := r.Facet()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO ` + table("content_items") + ` (tenant_id, id, kind) VALUES ($1, $2, 'post')`, []any{tenant, c}},
			{`INSERT INTO ` + table("content_versions") + ` (tenant_id, id, content_id, languages) VALUES ($1, $2, $3, '{en}')`, []any{tenant, v, c}},
			{`INSERT INTO ` + table("content_facets") + ` (tenant_id, content_id, version_id, kind, languages, access_level, access_tiers, access_group_id, access_releases)
				VALUES ($1, $2, $3, 'post', '{en}', $4, $5, $6, $7)`, []any{tenant, c, v, f.Level, f.Tiers, f.GroupID, f.Releases}},
		} {
			if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		items = append(items, access.Item{Content: c, Version: v, Rule: r})
	}
	tiers := []string{"gold", "premium"}
	expr, args, err := access.AudiencesSQL("f", tiers)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT f.version_id, `+expr+` FROM `+table("content_facets")+` f`, pgx.NamedArgs(args))
	if err != nil {
		t.Fatalf("%v\n%s", err, expr)
	}
	got := map[string][]string{}
	for rows.Next() {
		var v string
		var aud []string
		if err := rows.Scan(&v, &aud); err != nil {
			t.Fatal(err)
		}
		slices.Sort(aud)
		got[v] = aud
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	g, err := access.NewGate(access.GateConfig{Tenant: tenant, Kinds: kinds.Config{Tiers: tiers, TokenClaims: tiers}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{}
	for _, audience := range append([]string{access.Anyone}, tiers...) {
		viewer := access.Actor{Anonymous: true}
		if audience != access.Anyone {
			viewer = access.Actor{ID: "u", Kind: "user", Verified: claims{audience}}
		}
		ds, err := g.Decide(ctx, viewer, items)
		if err != nil {
			t.Fatal(err)
		}
		for i, d := range ds {
			if d.Allowed {
				want[items[i].Version] = append(want[items[i].Version], audience)
			}
		}
	}
	for i, it := range items {
		w := want[it.Version]
		slices.Sort(w)
		if strings.Join(got[it.Version], ",") != strings.Join(w, ",") {
			t.Errorf("rule %d %+v: audiences %q, want %q", i, it.Rule, got[it.Version], w)
		}
	}
	for _, bad := range [][]string{{"Gold"}, {"gold", "gold"}} {
		if _, _, err := access.AudiencesSQL("f", bad); err == nil {
			t.Errorf("tiers %v accepted", bad)
		}
	}
	if _, _, err := access.AudiencesSQL("f)", tiers); err == nil {
		t.Error("unsafe alias accepted")
	}
}
