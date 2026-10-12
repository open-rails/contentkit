package access_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
)

// Columns.Audiences names, per row, exactly the audiences that open it
// without owning it: an anonymous visitor (Anyone) or a holder of one tier,
// as grants.truth decides with nothing owned and no membership.
func TestAudiencesMatchTruth(t *testing.T) {
	pool, table, rows := fixture(t)
	tiers := []string{"premium", "gold"}
	for _, cols := range []access.Columns{fixedTier, rowTier} {
		audiences, args, err := cols.Audiences(tiers)
		if err != nil {
			t.Fatal(err)
		}
		q, err := pool.Query(t.Context(), "SELECT p.id::text, "+audiences+" FROM "+table+" p", pgx.NamedArgs(args))
		if err != nil {
			t.Fatalf("%v\n%s", err, audiences)
		}
		got := map[string][]string{}
		for q.Next() {
			var id string
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
			for _, a := range append([]string{access.Anyone}, tiers...) {
				var held []string
				if a != access.Anyone {
					held = []string{a}
				}
				if (grants{tiers: held}).truth(cols, r, i) {
					want = append(want, a)
				}
			}
			slices.Sort(want)
			if strings.Join(got[r.id], ",") != strings.Join(want, ",") {
				t.Errorf("%s: row %d %s/%s: audiences %q want %q", cols.Level, i, r.value, r.tier, got[r.id], want)
			}
		}
	}
	for name, c := range map[string]struct {
		cols  access.Columns
		tiers []string
	}{
		"no levels":         {access.Columns{Level: "p.access_policy"}, tiers},
		"both tier sources": {access.Columns{Level: "l", Levels: fixedTier.Levels, Tier: "t", TierName: "premium"}, tiers},
		"tier unset":        {access.Columns{Level: "l", Levels: rowTier.Levels}, tiers},
		"tier name absent":  {fixedTier, []string{"gold"}},
		"bad tier":          {rowTier, []string{"Gold"}},
		"repeated tier":     {rowTier, []string{"gold", "gold"}},
		"bad level":         {access.Columns{Level: "l", Levels: map[string]access.Level{"x": "bogus"}}, tiers},
		"bad prefix":        {access.Columns{Level: "l", Levels: fixedTier.Levels, TierName: "premium", ArgPrefix: "X"}, tiers},
	} {
		if _, _, err := c.cols.Audiences(c.tiers); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
