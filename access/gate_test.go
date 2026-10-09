package access_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/access/accesstest"
	"github.com/open-rails/contentkit/contentref"
)

const tenant = "onlydemo"

func uid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func post(n int) contentref.ContentRef    { return contentref.New(tenant, "post", uid(n)) }
func channel(n int) contentref.ContentRef { return contentref.New(tenant, "channel", uid(1000+n)) }

var viewer = access.Actor{ID: "0192aaaa-0000-7000-8000-000000000001", Kind: "user"}

type world struct {
	held  *accesstest.Entitlements
	count *accesstest.CountingEntitlements
	gate  *access.Gate
}

func newWorld(t *testing.T, mutate func(*access.GateConfig)) *world {
	t.Helper()
	w := &world{held: &accesstest.Entitlements{}}
	w.count = &accesstest.CountingEntitlements{Next: w.held}
	cfg := access.GateConfig{Entitlements: w.count, Tenant: tenant, Tiers: []string{"premium", "gold"},
		MemberKinds: []string{"channel"}, OwnedKinds: []string{"post"}}
	if mutate != nil {
		mutate(&cfg)
	}
	g, err := access.NewGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w.gate = g
	return w
}

func (w *world) grant(keys ...access.Key) {
	for _, k := range keys {
		w.held.Grant(viewer.ID, k.String())
	}
}

func (w *world) calls(t *testing.T, want int) {
	t.Helper()
	if got := w.count.Calls(); got != want {
		t.Fatalf("billing reads = %d, want %d (%+v)", got, want, w.count.Queries())
	}
}

// page is 50 items over every level, two channels and both tiers.
func page() []access.Item {
	var items []access.Item
	for i := range 50 {
		var r access.Rule
		switch i % 5 {
		case 0:
			r = access.Rule{Level: access.Public}
		case 1:
			r = access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
		case 2:
			r = access.Rule{Level: access.MembersLevel, Scope: channel(i % 2)}
		case 3:
			r = access.Rule{Level: access.PPV}
		case 4:
			r = access.Rule{Level: access.MembersPPV, Scope: channel(i % 2)}
		}
		items = append(items, access.Item{Ref: post(i), Rule: r})
	}
	return items
}

func TestDecideFiftyItemsIsOneRead(t *testing.T) {
	w := newWorld(t, nil)
	w.grant(access.Members(channel(0)), access.Own(post(3)))
	ds, err := w.gate.Decide(t.Context(), viewer, page())
	if err != nil {
		t.Fatal(err)
	}
	w.calls(t, 1)
	if q := w.count.Queries()[0]; len(q.Prefixes) != 0 || len(q.Keys) == 0 {
		t.Fatalf("Decide reads exact keys only: %+v", q)
	}
	for i, d := range ds {
		want := i%5 == 0 || i == 3 || (i%5 == 2 && i%2 == 0)
		if d.Allowed != want || d.Unknown {
			t.Errorf("item %d: %+v, want allowed=%v", i, d, want)
		}
	}
	// Anonymous viewers never reach billing.
	w.count.Reset()
	ds, err = w.gate.Decide(t.Context(), access.Actor{Anonymous: true}, page())
	if err != nil {
		t.Fatal(err)
	}
	w.calls(t, 0)
	for i, d := range ds {
		if d.Allowed != (i%5 == 0) || d.Unknown {
			t.Errorf("anonymous item %d: %+v", i, d)
		}
	}
}

func TestFilterThenDecideWithMemoIsOneRead(t *testing.T) {
	w := newWorld(t, nil)
	w.grant(access.Tier("gold"), access.Members(channel(1)), access.Own(post(8)))
	ctx := access.WithMemo(t.Context())
	if access.WithMemo(ctx) != ctx {
		t.Fatal("WithMemo must keep an existing memo")
	}
	f, err := w.gate.Filter(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if f.Tier("premium") || !f.Tier("gold") || !f.Complete("post") || !f.Complete("channel") || f.Complete("video") {
		t.Fatalf("filter: premium=%v gold=%v", f.Tier("premium"), f.Tier("gold"))
	}
	if got := f.Owned("post"); len(got) != 1 || got[0] != uid(8) {
		t.Fatalf("owned %v", got)
	}
	if got := f.Members("channel"); len(got) != 1 || got[0] != channel(1).ContentID {
		t.Fatalf("members %v", got)
	}
	again, _ := w.gate.Filter(ctx, viewer)
	if again != f {
		t.Fatal("memo must return the same filter")
	}
	ds, err := w.gate.Decide(ctx, viewer, page())
	if err != nil {
		t.Fatal(err)
	}
	w.calls(t, 1) // Filter's read decides every item of the page
	for i, d := range ds {
		want := i%5 == 0 || i == 8 || (i%5 == 2 && i%2 == 1)
		if d.Allowed != want {
			t.Errorf("item %d: %+v, want %v", i, d, want)
		}
		if allowed, known := f.Allows(page()[i]); !known || allowed != want {
			t.Errorf("Allows item %d = %v %v", i, allowed, known)
		}
	}
	if q := w.count.Queries()[0]; len(q.Keys) != 2 || len(q.Prefixes) != 2 || q.Limit != access.DefaultHeldLimit {
		t.Fatalf("Filter query %+v", q)
	}

	// Without a memo the same two calls are two reads, and correct.
	w.count.Reset()
	if _, err := w.gate.Filter(t.Context(), viewer); err != nil {
		t.Fatal(err)
	}
	if _, err := w.gate.Decide(t.Context(), viewer, page()); err != nil {
		t.Fatal(err)
	}
	w.calls(t, 2)

	// Keys Decide reads are memoized too: a kind the filter does not cover
	// costs one read per request, not one per call.
	w.count.Reset()
	ctx = access.WithMemo(t.Context())
	video := access.Item{Ref: contentref.New(tenant, "video", uid(7)), Rule: access.Rule{Level: access.PPV}}
	for range 3 {
		if _, err := w.gate.Decide(ctx, viewer, []access.Item{video}); err != nil {
			t.Fatal(err)
		}
	}
	w.calls(t, 1)
}

func TestSettleTruncatedKeyspace(t *testing.T) {
	w := newWorld(t, func(c *access.GateConfig) { c.HeldLimit = 2 })
	w.grant(access.Own(post(1)), access.Own(post(2)), access.Own(post(3)))
	f, err := w.gate.Filter(t.Context(), viewer)
	if err != nil {
		t.Fatal(err)
	}
	if f.Complete("post") || len(f.Owned("post")) != 2 {
		t.Fatalf("complete=%v owned=%v", f.Complete("post"), f.Owned("post"))
	}
	ppv := func(n int) access.Item { return access.Item{Ref: post(n), Rule: access.Rule{Level: access.PPV}} }
	items := []access.Item{ppv(1), ppv(3), ppv(4), {Ref: post(5), Rule: access.Rule{Level: access.Public}}}
	if a, k := f.Allows(ppv(3)); a || k {
		t.Fatalf("a truncated keyspace cannot decide post 3: %v %v", a, k)
	}
	w.count.Reset()
	keep, short, err := w.gate.Settle(t.Context(), viewer, f, items)
	if err != nil {
		t.Fatal(err)
	}
	w.calls(t, 1)
	if q := w.count.Queries()[0]; len(q.Keys) != 2 || len(q.Prefixes) != 0 {
		t.Fatalf("Settle reads only the candidates' keys: %+v", q)
	}
	if fmt.Sprint(keep) != "[true true false true]" || !short {
		t.Fatalf("keep %v short %v", keep, short)
	}

	// A complete filter settles in memory.
	w2 := newWorld(t, nil)
	w2.held = w.held
	w2.count.Next = w.held
	f2, _ := w2.gate.Filter(t.Context(), viewer)
	w2.count.Reset()
	keep, short, err = w2.gate.Settle(t.Context(), viewer, f2, items[:2])
	if err != nil || fmt.Sprint(keep) != "[true true]" || short {
		t.Fatalf("keep %v short %v err %v", keep, short, err)
	}
	w2.calls(t, 0)
	if _, _, err := w.gate.Settle(t.Context(), viewer, f2, items); err == nil {
		t.Fatal("Settle must refuse another gate's filter")
	}
}

func TestDecideSellsAndRequires(t *testing.T) {
	w := newWorld(t, nil)
	mppv := access.Item{Ref: post(1), Rule: access.Rule{Level: access.MembersPPV, Scope: channel(1)}}
	tier := access.Item{Ref: post(2), Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"premium", "gold"}}}
	members := access.Item{Ref: post(3), Rule: access.Rule{Level: access.MembersLevel, Scope: channel(1)}}
	ds, err := w.gate.Decide(t.Context(), viewer, []access.Item{mppv, tier, members})
	if err != nil {
		t.Fatal(err)
	}
	if d := ds[0]; d.Allowed || d.Unknown || len(d.Sell) != 1 || d.Sell[0] != access.Own(post(1)) || d.Requires == nil || *d.Requires != access.Members(channel(1)) {
		t.Fatalf("members_ppv without membership: %+v", d)
	}
	if d := ds[1]; d.Allowed || len(d.Sell) != 2 || d.Sell[0] != access.Tier("premium") || d.Requires != nil {
		t.Fatalf("tier: %+v", d)
	}
	if d := ds[2]; d.Allowed || len(d.Sell) != 1 || d.Sell[0] != access.Members(channel(1)) {
		t.Fatalf("members: %+v", d)
	}
	w.grant(access.Members(channel(1)))
	ds, _ = w.gate.Decide(t.Context(), viewer, []access.Item{mppv, members})
	if d := ds[0]; d.Allowed || d.Requires != nil || len(d.Sell) != 1 {
		t.Fatalf("members_ppv with membership: %+v", d)
	}
	if !ds[1].Allowed {
		t.Fatal("member denied")
	}
	// Anonymous viewers are offered the same, with no read.
	w.count.Reset()
	ds, _ = w.gate.Decide(t.Context(), access.Actor{Anonymous: true}, []access.Item{mppv})
	if d := ds[0]; d.Allowed || d.Unknown || d.Requires == nil {
		t.Fatalf("anonymous members_ppv: %+v", d)
	}
	w.calls(t, 0)
}

func TestUnknownFailsClosedAndSellsNothing(t *testing.T) {
	w := newWorld(t, nil)
	w.grant(access.Own(post(1)), access.Tier("premium"))
	boom := errors.New("billing down")
	w.held.Fail(boom)
	items := []access.Item{
		{Ref: post(0), Rule: access.Rule{Level: access.Public}},
		{Ref: post(1), Rule: access.Rule{Level: access.PPV}},
		{Ref: post(2), Rule: access.Rule{Level: access.MembersPPV, Scope: channel(1)}},
		{Ref: post(3), Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}},
	}
	ds, err := w.gate.Decide(t.Context(), viewer, items)
	if !errors.Is(err, access.ErrUnavailable) || !errors.Is(err, boom) || len(ds) != 4 {
		t.Fatalf("Decide: %v %v", ds, err)
	}
	if !ds[0].Allowed || ds[0].Unknown {
		t.Fatalf("public must keep serving: %+v", ds[0])
	}
	for _, d := range ds[1:] {
		if d.Allowed || !d.Unknown || d.Sell != nil || d.Requires != nil {
			t.Fatalf("keyed item must be denied and not for sale: %+v", d)
		}
	}
	f, err := w.gate.Filter(t.Context(), viewer)
	if !errors.Is(err, access.ErrUnavailable) || f == nil || !f.Unknown() || f.Complete("post") || f.Tier("premium") {
		t.Fatalf("Filter: %+v %v", f, err)
	}
	for i, it := range items {
		if a, k := f.Allows(it); a != (i == 0) || k != (i == 0) {
			t.Errorf("unknown filter Allows %d = %v %v", i, a, k)
		}
	}
	// The memo keeps the failure for the request: no retry storm per item.
	ctx := access.WithMemo(t.Context())
	w.count.Reset()
	for range 3 {
		if _, err := w.gate.Filter(ctx, viewer); !errors.Is(err, access.ErrUnavailable) {
			t.Fatal(err)
		}
	}
	w.calls(t, 1)
}

func TestClaimsShortcut(t *testing.T) {
	var claims []string
	w := newWorld(t, func(c *access.GateConfig) {
		c.MemberKinds, c.OwnedKinds = nil, nil
		c.Claims = func(context.Context) []string { return claims }
	})
	tier := access.Item{Ref: post(1), Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}}

	// A claimed tier is held with no read, in Filter and in Decide.
	claims = []string{"premium", "gold", "content:onlydemo:post:" + uid(9), "unknown_tier"}
	f, err := w.gate.Filter(t.Context(), viewer)
	if err != nil || !f.Tier("premium") || !f.Tier("gold") {
		t.Fatalf("claimed filter: %v", err)
	}
	ds, err := w.gate.Decide(t.Context(), viewer, []access.Item{tier})
	if err != nil || !ds[0].Allowed {
		t.Fatalf("claimed decide: %+v %v", ds, err)
	}
	w.calls(t, 0)

	// Claims never vouch for item keys.
	w.count.Reset()
	ds, _ = w.gate.Decide(t.Context(), viewer, []access.Item{{Ref: post(9), Rule: access.Rule{Level: access.PPV}}})
	if ds[0].Allowed {
		t.Fatal("an own key in claims must not unlock")
	}
	w.calls(t, 1)

	// An absent claim falls through to a live read: a fresh purchase shows at once.
	claims = []string{"gold"}
	w.grant(access.Tier("premium"))
	w.count.Reset()
	f, _ = w.gate.Filter(t.Context(), viewer)
	if !f.Tier("premium") {
		t.Fatal("live premium missed")
	}
	if q := w.count.Queries(); len(q) != 1 || fmt.Sprint(q[0].Keys) != "[premium]" {
		t.Fatalf("only the unclaimed tier is read: %+v", q)
	}
	ds, _ = w.gate.Decide(t.Context(), viewer, []access.Item{tier})
	if !ds[0].Allowed {
		t.Fatal("live decide")
	}
	w.calls(t, 2)
}

func TestGateRefusesInvalidInput(t *testing.T) {
	w := newWorld(t, nil)
	for _, it := range []access.Item{
		{Ref: contentref.New("other", "post", uid(1)), Rule: access.Rule{Level: access.PPV}},
		{Ref: contentref.NewVersion(tenant, "post", uid(1), "v1"), Rule: access.Rule{Level: access.PPV}},
		{Ref: post(1), Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"platinum"}}},
		{Ref: post(1), Rule: access.Rule{Level: access.MembersLevel, Scope: contentref.New("other", "channel", uid(2))}},
		{Ref: post(1), Rule: access.Rule{Level: "vip"}},
	} {
		if ds, err := w.gate.Decide(t.Context(), viewer, []access.Item{it}); err == nil || ds != nil {
			t.Errorf("Decide(%+v) = %v, %v", it, ds, err)
		}
	}
	w.calls(t, 0)
	held := &accesstest.Entitlements{}
	for _, c := range []access.GateConfig{
		{Tenant: tenant},
		{Entitlements: held},
		{Entitlements: held, Tenant: "Bad"},
		{Entitlements: held, Tenant: tenant, Tiers: []string{"premium", "premium"}},
		{Entitlements: held, Tenant: tenant, Tiers: []string{"Premium"}},
		{Entitlements: held, Tenant: tenant, OwnedKinds: []string{"po:st"}},
		{Entitlements: held, Tenant: tenant, HeldLimit: access.MaxHeldLimit + 1},
		{Entitlements: held, Tenant: tenant, HeldLimit: -1},
		{Entitlements: held, Tenant: tenant, OwnedKinds: []string{"a", "b", "c", "d", "e", "f"}, MemberKinds: []string{"g", "h", "i", "j", "k"}},
	} {
		if _, err := access.NewGate(c); err == nil {
			t.Errorf("NewGate(%+v) accepted", c)
		}
	}
	if fmt.Sprint(w.gate.TierKeys()) != "[gold premium]" {
		t.Fatalf("TierKeys %v", w.gate.TierKeys())
	}
}
