package access_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
)

const tenant = "doujins"

func uid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func key(k access.Key, err error) access.Key {
	if err != nil {
		panic(err)
	}
	return k
}

// claims is a Verified carrying token entitlement claims (access.Entitled).
type claims []string

func (claims) Identity() auth.Identity  { return auth.Identity{Subject: "u"} }
func (c claims) Entitlements() []string { return c }

func newGate(t *testing.T) *access.Gate {
	t.Helper()
	g, err := access.NewGate(access.GateConfig{Tenant: tenant, Kinds: kinds.Config{Tiers: []string{"premium", "gold"}, TokenClaims: []string{"premium"}}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Without Billing nobody holds a key: free rules open, claimed tiers open
// from the token alone, and every locked version offers what opens it.
func TestGateWithoutBilling(t *testing.T) {
	g := newGate(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	c, v, grp := uid(1), uid(2), uid(3)
	items := []access.Item{
		{Content: c, Version: v, Rule: access.Rule{Level: access.Public}},
		{Content: c, Version: v, Rule: access.Rule{Level: access.PPV, Releases: []access.Release{{At: past, Level: access.Public}}}},
		{Content: c, Version: v, Rule: access.Rule{Level: access.PPV, Releases: []access.Release{{At: future, Level: access.Public}}, Preview: &access.Preview{Pages: 2}}},
		{Content: c, Version: v, Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"gold", "premium", "platinum"}}},
		{Content: c, Rule: access.Rule{Level: access.GroupLevel, Group: grp}},
		{Content: c, Version: v, Rule: access.Rule{Level: access.GroupPPV, Group: grp}, Groups: []string{uid(4)}},
		{Content: "nope", Rule: access.Rule{Level: access.PPV}},
		{Content: c, Rule: access.Rule{Level: "vip"}},
	}
	ck, vk, gk := key(access.ContentKey(tenant, c)), key(access.VersionKey(tenant, c, v)), key(access.GroupKey(tenant, grp))
	gold, premium := key(access.TierKey("gold")), key(access.TierKey("premium"))
	for _, viewer := range []access.Actor{{Anonymous: true}, {ID: "u", Kind: auth.SubjectUser}} {
		ds, err := g.Decide(t.Context(), viewer, items)
		if err != nil {
			t.Fatal(err)
		}
		want := []access.Decision{
			{Allowed: true}, {Allowed: true},
			{Sell: []access.Key{vk, ck}, Preview: &access.Preview{Pages: 2}, Releases: []access.Release{{At: future, Level: access.Public}}},
			{Sell: []access.Key{gold, premium, vk, ck}},
			{Sell: []access.Key{gk, ck}},
			{Sell: []access.Key{vk, ck}, Requires: &gk},
			{}, {},
		}
		for i, d := range ds {
			if !same(d, want[i]) {
				t.Errorf("%+v item %d: %+v, want %+v", viewer, i, d, want[i])
			}
		}
	}
	// A claimable tier in the token opens without Billing; gold is not claimable.
	viewer := access.Actor{ID: "u", Kind: auth.SubjectUser, Verified: claims{"premium", "gold"}}
	ds, err := g.Decide(t.Context(), viewer, []access.Item{
		{Content: c, Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}},
		{Content: c, Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}},
	})
	if err != nil || !ds[0].Allowed || ds[1].Allowed {
		t.Fatalf("claims: %+v %v", ds, err)
	}
	f, err := g.Filter(t.Context(), viewer)
	if err != nil || !f.Tier("premium") || f.Tier("gold") || !f.Complete() {
		t.Fatalf("filter %v", err)
	}
	if _, args, err := f.SQL("f", access.Hide); err != nil || fmt.Sprint(args["access_tiers"]) != "[premium]" {
		t.Fatalf("SQL args %v %v", args, err)
	}
	if _, _, err := f.SQL("f; drop", access.Hide); err == nil {
		t.Fatal("unsafe alias accepted")
	}
}

func TestNewGateRefuses(t *testing.T) {
	for _, c := range []access.GateConfig{
		{Tenant: ""}, {Tenant: "Doujins"}, {Tenant: "a:b"},
		{Tenant: tenant, Kinds: kinds.Config{Tiers: []string{"premium", "premium"}}},
		{Tenant: tenant, Kinds: kinds.Config{Tiers: []string{"Premium"}}},
		{Tenant: tenant, Kinds: kinds.Config{Tiers: []string{"premium"}, TokenClaims: []string{"gold"}}},
	} {
		if _, err := access.NewGate(c); err == nil {
			t.Errorf("NewGate(%+v) accepted", c)
		}
	}
}

func same(a, b access.Decision) bool {
	return a.Allowed == b.Allowed && a.Unknown == b.Unknown && slices.Equal(a.Sell, b.Sell) && equalPtr(a.Requires, b.Requires) &&
		equalPtr(a.Preview, b.Preview) && reflect.DeepEqual(a.Releases, b.Releases)
}

func equalPtr[T comparable](a, b *T) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
