package access_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
)

// Columns.Audiences names, per row, exactly the audiences that open it
// without buying it: an anonymous visitor (Anyone) for a public row, and a
// holder of the row's tier for a tier row.
func TestAudiencesMatchTruth(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	table := pgx.Identifier{pgtest.EmptySchema(t, ctx, pool), "posts"}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id int PRIMARY KEY, access_policy text NOT NULL, tier text)`); err != nil {
		t.Fatal(err)
	}
	type row struct{ value, tier string }
	var rows []row
	for _, value := range []string{"public", "premium", "tier", "ppv", "bogus"} {
		for _, tier := range []string{"premium", "gold", "undeclared"} {
			rows = append(rows, row{value, tier})
			if _, err := pool.Exec(ctx, `INSERT INTO `+table+` VALUES ($1, $2, $3)`, len(rows), value, tier); err != nil {
				t.Fatal(err)
			}
		}
	}
	levels := map[string]access.Level{"public": access.Public, "premium": access.TierLevel, "tier": access.TierLevel, "ppv": access.PPV}
	fixed := access.Columns{Level: "p.access_policy", TierName: "premium", Levels: levels}
	perRow := access.Columns{Level: "lower(p.access_policy)", Tier: "p.tier", Levels: levels, ArgPrefix: "acc_"}
	tiers := []string{"premium", "gold"}
	for _, cols := range []access.Columns{fixed, perRow} {
		audiences, args, err := cols.Audiences(tiers)
		if err != nil {
			t.Fatal(err)
		}
		q, err := pool.Query(ctx, "SELECT p.id, "+audiences+" FROM "+table+" p ORDER BY p.id", pgx.NamedArgs(args))
		if err != nil {
			t.Fatalf("%v\n%s", err, audiences)
		}
		got := map[int][]string{}
		for q.Next() {
			var id int
			var aud []string
			if err := q.Scan(&id, &aud); err != nil {
				t.Fatal(err)
			}
			slices.Sort(aud)
			got[id] = aud
		}
		if err := q.Err(); err != nil {
			t.Fatal(err)
		}
		for i, r := range rows {
			var want []string
			switch levels[r.value] {
			case access.Public:
				want = append([]string{access.Anyone}, tiers...)
			case access.TierLevel:
				tier := r.tier
				if cols.TierName != "" {
					tier = cols.TierName
				}
				if slices.Contains(tiers, tier) {
					want = []string{tier}
				}
			}
			slices.Sort(want)
			if strings.Join(got[i+1], ",") != strings.Join(want, ",") {
				t.Errorf("%s: row %d %+v: audiences %q want %q", cols.Level, i+1, r, got[i+1], want)
			}
		}
	}
	for name, c := range map[string]struct {
		cols  access.Columns
		tiers []string
	}{
		"no levels":         {access.Columns{Level: "p.access_policy"}, tiers},
		"both tier sources": {access.Columns{Level: "l", Levels: levels, Tier: "t", TierName: "premium"}, tiers},
		"tier unset":        {access.Columns{Level: "l", Levels: levels}, tiers},
		"tier name absent":  {fixed, []string{"gold"}},
		"bad tier":          {perRow, []string{"Gold"}},
		"repeated tier":     {perRow, []string{"gold", "gold"}},
		"bad level":         {access.Columns{Level: "l", Levels: map[string]access.Level{"x": "bogus"}}, tiers},
		"bad prefix":        {access.Columns{Level: "l", Levels: levels, TierName: "premium", ArgPrefix: "X"}, tiers},
	} {
		if _, _, err := c.cols.Audiences(c.tiers); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
