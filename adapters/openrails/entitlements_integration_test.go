package openrails_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/contentref"
)

const tenant = "onlydemo"

// counting counts the CheckEntitlements requests the adapter sends.
type counting struct {
	c *openrails.Client
	n atomic.Int64
}

func (c *counting) CheckEntitlements(ctx context.Context, id billing.CustomerID, p billing.CheckEntitlementsParams, o ...openrails.RequestOption) (*billing.EntitlementCheck, error) {
	c.n.Add(1)
	return c.c.CheckEntitlements(ctx, id, p, o...)
}

func (c *counting) take() int { return int(c.n.Swap(0)) }

// embedded runs OpenRails in process on a fresh PostgreSQL schema
// (CONTENTKIT_TEST_URL; CONTENTKIT_TEST_REQUIRE_DB=1 fails instead of skipping).
func embedded(t *testing.T) *openrails.Client {
	t.Helper()
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		if os.Getenv("CONTENTKIT_TEST_REQUIRE_DB") == "1" {
			t.Fatal("CONTENTKIT_TEST_URL is required")
		}
		t.Skip("CONTENTKIT_TEST_URL not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ckor_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	cfg := openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}, TestMode: openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesReadOnly, Merchant: openrails.MerchantDeclaration{Slug: tenant, DisplayName: "OnlyDemo"}}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

func uid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func ref(kind string, n int) contentref.ContentRef { return contentref.New(tenant, kind, uid(n)) }

// One Filter is one CheckEntitlements against a real embedded OpenRails: the
// declared tiers and the held keys per keyspace, byte-exact, truncated past
// HeldLimit; Decide after Filter in one request costs nothing more.
func TestEntitlementsOverEmbeddedOpenRails(t *testing.T) {
	ctx := t.Context()
	bill := embedded(t)
	customer := billing.CustomerID(uuid.New())
	if _, err := bill.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}}); err != nil {
		t.Fatal(err)
	}
	grant := func(keys ...string) {
		product, err := bill.CreateProduct(ctx, billing.CreateProductParams{Key: "fixture", DisplayName: "Fixture", Entitlements: keys})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bill.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{
			Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}}); err != nil {
			t.Fatal(err)
		}
	}
	grant(access.Tier("premium").String(), access.Members(ref("channel", 1)).String(),
		access.Own(ref("post", 3)).String(), access.Own(ref("post", 1)).String(), access.Own(ref("post", 2)).String(),
		access.Own(contentref.New("hentai0", "post", uid(4))).String(), // another tenant's keyspace
		"content:"+tenant+":post:not-a-content-id",                     // under the prefix, outside the grammar
		access.Own(ref("video", 7)).String())
	client := &counting{c: bill}
	viewer := access.Actor{ID: customer.String(), Kind: "user"}
	newGate := func(limit int) *access.Gate {
		g, err := access.NewGate(access.GateConfig{Entitlements: ckopenrails.Entitlements(client), Tenant: tenant,
			Tiers: []string{"premium", "gold"}, MemberKinds: []string{"channel"}, OwnedKinds: []string{"post"}, HeldLimit: limit})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	gate := newGate(0)

	f, err := gate.Filter(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if n := client.take(); n != 1 {
		t.Fatalf("Filter sent %d requests, want 1", n)
	}
	if !f.Tier("premium") || f.Tier("gold") || !f.Complete("post") || !f.Complete("channel") {
		t.Fatalf("tiers premium=%v gold=%v complete=%v/%v", f.Tier("premium"), f.Tier("gold"), f.Complete("post"), f.Complete("channel"))
	}
	if got := f.Owned("post"); !slices.Equal(got, []string{uid(1), uid(2), uid(3)}) {
		t.Fatalf("owned %v", got)
	}
	if got := f.Members("channel"); !slices.Equal(got, []string{uid(1)}) {
		t.Fatalf("members %v", got)
	}

	// Filter then Decide in one request is one request in all.
	var page []access.Item
	for i := range 50 {
		r := access.Rule{Level: access.PPV}
		if i%2 == 1 {
			r = access.Rule{Level: access.MembersLevel, Scope: ref("channel", i%3)}
		}
		page = append(page, access.Item{Ref: ref("post", i), Rule: r})
	}
	mctx := access.WithMemo(ctx)
	if _, err := gate.Filter(mctx, viewer); err != nil {
		t.Fatal(err)
	}
	ds, err := gate.Decide(mctx, viewer, page)
	if err != nil {
		t.Fatal(err)
	}
	if n := client.take(); n != 1 {
		t.Fatalf("Filter + Decide sent %d requests, want 1", n)
	}
	for i, d := range ds {
		if want := (i >= 1 && i <= 3) || (i%2 == 1 && i%3 == 1); d.Allowed != want {
			t.Errorf("post %d: %+v, want %v", i, d, want)
		}
	}

	// Past HeldLimit the keyspace is incomplete; Settle decides the rest in one request.
	small := newGate(2)
	f, err = small.Filter(ctx, viewer)
	if err != nil || f.Complete("post") || len(f.Owned("post")) != 2 {
		t.Fatalf("truncated filter: %v complete=%v owned=%v", err, f.Complete("post"), f.Owned("post"))
	}
	keep, short, err := small.Settle(ctx, viewer, f, page[:5])
	if err != nil || fmt.Sprint(keep) != "[false true true true false]" || !short {
		t.Fatalf("Settle keep=%v short=%v err=%v", keep, short, err)
	}
	if n := client.take(); n != 2 {
		t.Fatalf("Filter + Settle sent %d requests, want 2", n)
	}

	// Over billing.MaxEntitlementChecks keys, one Held is chunked (still correct).
	var videos []access.Item
	for i := range 150 {
		videos = append(videos, access.Item{Ref: ref("video", i), Rule: access.Rule{Level: access.PPV}})
	}
	ds, err = gate.Decide(ctx, viewer, videos)
	if err != nil {
		t.Fatal(err)
	}
	if n := client.take(); n != 2 {
		t.Fatalf("150 keys sent %d requests, want 2 chunks", n)
	}
	for i, d := range ds {
		if d.Allowed != (i == 7) {
			t.Errorf("video %d: %+v", i, d)
		}
	}

	// Anonymous viewers and subjects that are not customers cost nothing.
	for _, a := range []access.Actor{{Anonymous: true}, {ID: "service:indexer", Kind: "service"}} {
		f, err := gate.Filter(ctx, a)
		if err != nil || f.Tier("premium") || len(f.Owned("post")) != 0 || !f.Complete("post") {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	if n := client.take(); n != 0 {
		t.Fatalf("non-customers sent %d requests", n)
	}
}
