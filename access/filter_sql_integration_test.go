package access_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/access/accesstest"
	"github.com/open-rails/contentkit/internal/pgtest"
)

type fixtureRow struct {
	id, value, tier string
	channel         int
}

// Two listings over one table: a fixed tier (OnlyDemo-style access_policy
// with "premium" meaning the premium tier) and a per-row tier column.
var (
	fixedTier = access.Columns{ID: "p.id", Kind: "post", Level: "p.access_policy", TierName: "premium",
		Levels: map[string]access.Level{"public": access.Public, "premium": access.TierLevel, "membership": access.MembersLevel,
			"ppv": access.PPV, "members_ppv": access.MembersPPV},
		Scope: "p.channel_id", ScopeKind: "channel"}
	rowTier = access.Columns{ID: "p.id", Kind: "post", Level: "lower(p.access_policy)", Tier: "p.tier",
		Levels: map[string]access.Level{"public": access.Public, "tier": access.TierLevel, "membership": access.MembersLevel,
			"ppv": access.PPV, "members_ppv": access.MembersPPV},
		Scope: "p.channel_id", ScopeKind: "channel", ArgPrefix: "acc_"}
)

func fixture(t *testing.T) (*pgxpool.Pool, string, []fixtureRow) {
	t.Helper()
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	table := pgx.Identifier{schema, "posts"}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id uuid PRIMARY KEY, access_policy text NOT NULL,
		channel_id uuid NOT NULL, tier text, published_at timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	var rows []fixtureRow
	tiers := []string{"premium", "gold", "undeclared"}
	n := 0
	for _, value := range []string{"public", "premium", "tier", "membership", "ppv", "members_ppv", "bogus"} {
		for ch := range 3 {
			for copy := range 2 {
				n++
				r := fixtureRow{id: uid(n), value: value, channel: ch, tier: tiers[(ch+copy)%3]}
				rows = append(rows, r)
				if _, err := pool.Exec(ctx, `INSERT INTO `+table+` VALUES ($1, $2, $3, $4, $5)`,
					r.id, r.value, channel(ch).ContentID, r.tier, time.Unix(int64(n), 0)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return pool, table, rows
}

// item is the host's Go view of a row under c.
func item(c access.Columns, r fixtureRow) access.Item {
	rule := access.Rule{Level: c.Levels[r.value], Scope: channel(r.channel)}
	if rule.Level == access.TierLevel {
		rule.Tiers = []string{cmpOr(c.TierName, r.tier)}
	}
	return access.Item{Ref: post(idNumber(r.id)), Rule: rule}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func idNumber(id string) int {
	var n int
	_, _ = fmt.Sscanf(id[24:], "%d", &n)
	return n
}

type grants struct {
	tiers   []string
	members []int
	owned   []int // row indexes
}

// truth is a row's accessibility from the grants alone, independent of the Filter.
func (gr grants) truth(c access.Columns, r fixtureRow, idx int) bool {
	if _, mapped := c.Levels[r.value]; !mapped {
		return false // an unmapped value fails closed, owned or not
	}
	if slices.Contains(gr.owned, idx) {
		return true
	}
	switch c.Levels[r.value] {
	case access.Public:
		return true
	case access.TierLevel:
		return slices.Contains(gr.tiers, cmpOr(c.TierName, r.tier)) && (r.tier != "undeclared" || c.TierName != "")
	case access.MembersLevel:
		return slices.Contains(gr.members, r.channel)
	}
	return false
}

func selectIDs(t *testing.T, pool *pgxpool.Pool, sql string, args map[string]any) map[string]bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), sql, pgx.NamedArgs(args))
	if err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// For every combination of held tiers, memberships, owned items, truncation,
// an unknown read and an anonymous viewer, Filter.SQL and Filter.Branches
// select exactly the rows Allows allows or does not know (allows only, when
// Unknown); the SQL never hides an accessible row; and Settle trims the
// candidates to exactly the accessible ones in at most one read.
func TestFilterSQLMatchesAllows(t *testing.T) {
	pool, table, rows := fixture(t)
	held := &accesstest.Entitlements{}
	count := &accesstest.CountingEntitlements{Next: held}
	gate := func(limit int, claims []string) *access.Gate {
		g, err := access.NewGate(access.GateConfig{Entitlements: count, Tenant: tenant, Tiers: []string{"premium", "gold"},
			MemberKinds: []string{"channel"}, OwnedKinds: []string{"post"}, HeldLimit: limit,
			Claims: func(context.Context) []string { return claims }})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	type state struct {
		name    string
		gate    *access.Gate
		actor   access.Actor
		grants  grants
		unknown bool
	}
	var states []state
	subject := 0
	ownedSets := [][]int{nil, {0, 7, 14, 20, 27, 33, 40}, {13, 25, 35}}
	for _, tiers := range [][]string{nil, {"premium"}, {"gold"}, {"premium", "gold"}} {
		for _, members := range [][]int{nil, {0}, {0, 1}} {
			for _, owned := range ownedSets {
				for _, limit := range []int{0, 1} {
					subject++
					a := access.Actor{ID: fmt.Sprintf("0192bbbb-0000-7000-8000-%012d", subject)}
					gr := grants{tiers: tiers, members: members, owned: owned}
					for _, tr := range tiers {
						held.Grant(a.ID, access.Tier(tr).String())
					}
					for _, m := range members {
						held.Grant(a.ID, access.Members(channel(m)).String())
					}
					for _, i := range owned {
						held.Grant(a.ID, access.Own(post(idNumber(rows[i].id))).String())
					}
					states = append(states, state{fmt.Sprintf("tiers=%v members=%v owned=%v limit=%d", tiers, members, owned, limit), gate(limit, nil), a, gr, false})
				}
			}
		}
	}
	states = append(states, state{name: "anonymous", gate: gate(0, nil), actor: access.Actor{Anonymous: true}})
	down := access.Actor{ID: "0192cccc-0000-7000-8000-000000000001"}
	held.Grant(down.ID, access.Tier("premium").String(), access.Own(post(1)).String())
	states = append(states,
		state{name: "unknown", gate: gate(0, nil), actor: down, unknown: true, grants: grants{tiers: []string{"premium"}, owned: []int{0}}},
		state{name: "unknown with claims", gate: gate(0, []string{"premium"}), actor: down, unknown: true, grants: grants{tiers: []string{"premium"}, owned: []int{0}}})

	truncated, settled := 0, 0
	for _, st := range states {
		held.Fail(nil)
		if st.unknown {
			held.Fail(errors.New("billing down"))
		}
		f, err := st.gate.Filter(t.Context(), st.actor)
		if st.unknown != errors.Is(err, access.ErrUnavailable) || st.unknown != f.Unknown() || (err != nil && !st.unknown) {
			t.Fatalf("%s: Filter %v", st.name, err)
		}
		held.Fail(nil)
		if !f.Complete("post") && !st.unknown {
			truncated++
		}
		for _, cols := range []access.Columns{fixedTier, rowTier} {
			pred, args, err := f.SQL(cols)
			if err != nil {
				t.Fatal(err)
			}
			got := selectIDs(t, pool, "SELECT p.id::text FROM "+table+" p WHERE "+pred, args)
			branches, bargs, err := f.Branches(cols)
			if err != nil {
				t.Fatal(err)
			}
			union := map[string]bool{}
			if len(branches) > 0 {
				var qs []string
				for _, b := range branches {
					qs = append(qs, "SELECT p.id::text FROM "+table+" p WHERE "+b.SQL)
				}
				union = selectIDs(t, pool, strings.Join(qs, " UNION "), bargs)
			}
			var page []access.Item
			var truth []bool
			for i, r := range rows {
				it := item(cols, r)
				allowed, known := f.Allows(it)
				want := allowed || (!known && !f.Unknown())
				if got[r.id] != want || union[r.id] != want {
					t.Errorf("%s %s: row %d %s/%s/%s/ch%d: sql=%v branches=%v, Allows=%v,%v", st.name, cols.Level, i, r.id, r.value, r.tier, r.channel, got[r.id], union[r.id], allowed, known)
				}
				tr := st.grants.truth(cols, r, i)
				if f.Unknown() && got[r.id] && !tr {
					t.Errorf("%s %s: row %d admitted while billing is down", st.name, cols.Level, i)
				}
				if !f.Unknown() && tr && !got[r.id] {
					t.Errorf("%s %s: row %d accessible but hidden", st.name, cols.Level, i)
				}
				if got[r.id] && validRule(it) {
					page, truth = append(page, it), append(truth, tr)
				}
			}
			count.Reset()
			keep, short, err := st.gate.Settle(t.Context(), st.actor, f, page)
			if err != nil {
				t.Fatalf("%s: Settle %v", st.name, err)
			}
			if count.Calls() > 1 {
				t.Errorf("%s: Settle read %d times", st.name, count.Calls())
			}
			settled += count.Calls()
			if fmt.Sprint(keep) != fmt.Sprint(truth) || short != slices.Contains(truth, false) {
				t.Errorf("%s %s: Settle keep=%v short=%v, want %v", st.name, cols.Level, keep, short, truth)
			}
		}
	}
	if truncated == 0 || settled == 0 {
		t.Fatalf("no truncated state exercised Settle (truncated=%d settled=%d)", truncated, settled)
	}
}

// validRule excludes rows Decide refuses: unmapped levels and undeclared tiers.
func validRule(it access.Item) bool {
	if _, err := it.Rule.Unlocks(it.Ref); err != nil {
		return false
	}
	return !slices.Contains(it.Rule.Tiers, "undeclared")
}

// The hide-mode keyset page runs on the feed index in order, with the access
// predicate as a filter: no Seq Scan, no Sort. A channel page uses its
// channel index.
func TestFilterSQLKeysetUsesIndex(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	table := pgx.Identifier{schema, "feed"}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id uuid PRIMARY KEY, access_policy text NOT NULL, channel_id uuid NOT NULL, published_at timestamptz NOT NULL);
		INSERT INTO `+table+` SELECT uuidv7(), (ARRAY['public','premium','membership','ppv','members_ppv'])[1 + i % 5],
			('01920000-0000-7000-8000-' || lpad((1000 + i % 37)::text, 12, '0'))::uuid, now() - make_interval(secs => i)
		FROM generate_series(1, 20000) AS i;
		CREATE INDEX feed_idx ON `+table+` (published_at DESC, id DESC);
		CREATE INDEX feed_channel_idx ON `+table+` (channel_id, published_at DESC, id DESC);
		ANALYZE `+table); err != nil {
		t.Fatal(err)
	}
	held := &accesstest.Entitlements{}
	g, err := access.NewGate(access.GateConfig{Entitlements: held, Tenant: tenant, Tiers: []string{"premium"},
		MemberKinds: []string{"channel"}, OwnedKinds: []string{"post"}})
	if err != nil {
		t.Fatal(err)
	}
	held.Grant(viewer.ID, access.Members(channel(3)).String(), access.Own(post(1)).String(), access.Own(post(2)).String())
	f, err := g.Filter(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	pred, args, err := f.SQL(fixedTier)
	if err != nil {
		t.Fatal(err)
	}
	args["cursor_at"], args["cursor_id"], args["n"], args["channel"] = time.Now().Add(-time.Hour), "ffffffff-ffff-7fff-bfff-ffffffffffff", 51, channel(3).ContentID
	for _, tc := range []struct {
		where, index string
		ordered      bool
	}{
		{"", "feed_idx", true},
		// A small channel may be a bitmap scan of its index plus a top-N sort.
		{"p.channel_id = @channel AND ", "feed_channel_idx", false},
	} {
		q := `EXPLAIN (FORMAT JSON) SELECT p.id FROM ` + table + ` p WHERE ` + tc.where + `(p.published_at, p.id) < (@cursor_at, @cursor_id::uuid) AND ` +
			pred + ` ORDER BY p.published_at DESC, p.id DESC LIMIT @n`
		var plan []map[string]any
		if err := pool.QueryRow(ctx, q, pgx.NamedArgs(args)).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		nodes := map[string]bool{}
		var indexes []string
		var walk func(map[string]any)
		walk = func(n map[string]any) {
			nodes[fmt.Sprint(n["Node Type"])] = true
			if name, ok := n["Index Name"].(string); ok {
				indexes = append(indexes, name)
			}
			if kids, ok := n["Plans"].([]any); ok {
				for _, k := range kids {
					walk(k.(map[string]any))
				}
			}
		}
		walk(plan[0]["Plan"].(map[string]any))
		b, _ := json.MarshalIndent(plan, "", " ")
		ordered := !nodes["Sort"] && (nodes["Index Scan"] || nodes["Index Only Scan"])
		if nodes["Seq Scan"] || (tc.ordered && !ordered) || !slices.Contains(indexes, tc.index) {
			t.Fatalf("keyset page must scan %s (in order: %v):\n%s", tc.index, tc.ordered, b)
		}
		// And the page itself is full and correct.
		rows, err := pool.Query(ctx, strings.TrimPrefix(q, "EXPLAIN (FORMAT JSON) "), pgx.NamedArgs(args))
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[[16]byte])
		if err != nil || len(ids) != 51 {
			t.Fatalf("page of %d rows: %v", len(ids), err)
		}
	}
}
