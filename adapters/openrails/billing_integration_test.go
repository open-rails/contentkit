package openrails_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/access"
)

// One Filter is one CheckEntitlements against a real embedded OpenRails: the
// configured tiers and the tenant's content and group keyspaces, byte-exact,
// truncated past access.HeldLimit; Decide after Filter in one request costs
// nothing more; keys of other tenants or outside the grammar are ignored.
func TestFilterOverEmbeddedOpenRails(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	viewer := w.customer()
	w.holds(viewer, "premium", gk(uid(9)), ck(uid(3)), ck(uid(1)), vk(uid(2), uid(5)),
		must(access.ContentKey("hentai0", uid(4))).String(), // another tenant's keyspace
		tenant+":content:not-an-id")                         // under the prefix, outside the grammar

	f, err := w.gate.Filter(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if n := w.client.take(); n != 1 {
		t.Fatalf("Filter sent %d requests, want 1", n)
	}
	if !f.Tier("premium") || f.Tier("gold") || !f.Complete() {
		t.Fatalf("tiers premium=%v gold=%v complete=%v", f.Tier("premium"), f.Tier("gold"), f.Complete())
	}
	if got := f.Contents(); !slices.Equal(got, []string{uid(1), uid(3)}) {
		t.Fatalf("contents %v", got)
	}
	if got := f.Versions(); len(got) != 1 || got[uid(5)] != uid(2) {
		t.Fatalf("versions %v", got)
	}
	if got := f.Entities(); !slices.Equal(got, []string{uid(9)}) {
		t.Fatalf("groups %v", got)
	}

	// Filter then Decide in one request is one request in all.
	var page []access.Item
	for i := range 50 {
		it := access.Item{Content: uid(i), Version: uid(100 + i), Rule: access.Rule{Level: access.PPV}}
		if i%3 == 1 {
			it.Rule = access.Rule{Level: access.EntityLevel, Entity: uid(9)}
		}
		page = append(page, it)
	}
	mctx := access.WithMemo(ctx)
	if _, err := w.gate.Filter(mctx, viewer); err != nil {
		t.Fatal(err)
	}
	ds, err := w.gate.Decide(mctx, viewer, page)
	if err != nil {
		t.Fatal(err)
	}
	if n := w.client.take(); n != 1 {
		t.Fatalf("Filter + Decide sent %d requests, want 1", n)
	}
	for i, d := range ds {
		if want := i == 1 || i == 3 || i%3 == 1; d.Allowed != want {
			t.Errorf("item %d: %+v, want %v", i, d, want)
		}
	}

	// Decide alone reads only the page's keys, chunked past
	// billing.MaxEntitlementChecks; a second Decide in the request reads none.
	var many []access.Item
	for i := range 150 {
		many = append(many, access.Item{Content: uid(1000 + i), Rule: access.Rule{Level: access.PPV}})
	}
	mctx = access.WithMemo(ctx)
	if _, err := w.gate.Decide(mctx, viewer, many); err != nil {
		t.Fatal(err)
	}
	if _, err := w.gate.Decide(mctx, viewer, many[:10]); err != nil {
		t.Fatal(err)
	}
	if n := w.client.take(); n != 2 {
		t.Fatalf("150 keys sent %d requests, want 2 chunks", n)
	}

	// Anonymous viewers and subjects that are not customers cost nothing.
	for _, a := range []access.Actor{{Anonymous: true}, {ID: "service:indexer", Kind: "service"}} {
		f, err := w.gate.Filter(ctx, a)
		if err != nil || f.Tier("premium") || len(f.Contents()) != 0 || !f.Complete() {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	if n := w.client.take(); n != 0 {
		t.Fatalf("non-customers sent %d requests", n)
	}
}

// Past access.HeldLimit content keys the Filter is incomplete: Allows does
// not know the rest, SQL(Hide) admits them as candidates, and Settle decides
// a page in one exact read.
func TestFilterTruncated(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	viewer := w.customer()
	var keys []string
	for i := range access.HeldLimit + 1 {
		keys = append(keys, ck(uid(i)))
	}
	w.holds(viewer, keys...)
	f, err := w.gate.Filter(ctx, viewer)
	if err != nil || f.Complete() || len(f.Contents()) != access.HeldLimit {
		t.Fatalf("filter complete=%v contents=%d err=%v", f.Complete(), len(f.Contents()), err)
	}
	last := access.Item{Content: uid(access.HeldLimit), Rule: access.Rule{Level: access.PPV}}
	stranger := access.Item{Content: uid(5000), Rule: access.Rule{Level: access.PPV}}
	if _, known := f.Allows(last); known {
		t.Fatal("Allows knows a key past the limit")
	}
	if ok, known := f.Allows(access.Item{Content: uid(0), Rule: access.Rule{Level: access.PPV}}); !ok || !known {
		t.Fatal("Allows lost a read key")
	}
	w.client.take()
	keep, short, err := w.gate.Settle(ctx, viewer, f, []access.Item{last, stranger})
	if err != nil || fmt.Sprint(keep) != "[true false]" || !short {
		t.Fatalf("Settle keep=%v short=%v err=%v", keep, short, err)
	}
	if n := w.client.take(); n != 1 {
		t.Fatalf("Settle sent %d requests, want 1", n)
	}
}

// A batch carrying legacy PSPs on any price is refused before OpenRails
// creates a single price.
func TestCreatePricesRefusesPSPsAtomically(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	key := ck(uid(7))
	made, err := w.billing.CreateProducts(ctx, []access.NewProduct{{Key: key, DisplayName: "Atomic", Entitlements: []string{key}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.billing.CreatePrices(ctx, []access.NewPrice{
		{ProductID: made[0].ID, Key: "once", Amount: 500, Currency: "USD"},
		{ProductID: made[0].ID, Key: "legacy", Amount: 700, Currency: "USD", PSPs: []string{"nmi"}},
	})
	if err == nil {
		t.Fatal("legacy PSPs accepted")
	}
	got, err := w.billing.Products(ctx, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got[key].Prices); n != 0 {
		t.Fatalf("refused batch left %d prices", n)
	}
}
