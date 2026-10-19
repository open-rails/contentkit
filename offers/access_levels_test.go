package offers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestAccessPolicyRefusesIndividualOffers(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	cfg := kinds.Config{Languages: []string{"en"}, Tiers: []string{"premium"}, Kinds: map[string]kinds.Kind{
		"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, AccessLevels: []string{"public", "tier"}},
	}}
	billing := &heldForPolicyTest{}
	gate, err := access.NewGate(access.GateConfig{Tenant: "policy", Kinds: cfg, Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg, Billing: billing, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	id, version := contentref.NewID(), contentref.NewID()
	table := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	if _, err := pool.Exec(ctx, `INSERT INTO `+table("content_items")+` (tenant_id,id,kind) VALUES ('policy',$1,'gallery')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+table("content_versions")+` (tenant_id,id,content_id,languages) VALUES ('policy',$1,$2,ARRAY['en'])`, version, id); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{{Kind: ContentTarget, ID: id}, {Kind: VersionTarget, ID: version, Content: id}} {
		if _, err := s.Put(ctx, []Put{{Target: target, Terms: Terms{Amount: 499, Currency: "USD"}}}); !errors.Is(err, ErrConflict) {
			t.Fatalf("accepted sale for %s: %v", target.Kind, err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table("content_offers")+` WHERE tenant_id='policy'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("offers written=%d: %v", count, err)
	}
}

type policyBilling struct {
	access.Billing
	products map[string]access.Product
}

func (b *policyBilling) Products(_ context.Context, keys []string) (map[string]access.Product, error) {
	out := map[string]access.Product{}
	for _, key := range keys {
		if p, ok := b.products[key]; ok {
			out[key] = p
		}
	}
	return out, nil
}

func (b *policyBilling) UpdateProducts(_ context.Context, updates []access.ProductUpdate) ([]access.Product, error) {
	out := []access.Product{}
	for _, u := range updates {
		found := false
		for key, p := range b.products {
			if p.ID != u.ID {
				continue
			}
			if u.Archived != nil {
				p.Archived = *u.Archived
			}
			if u.DisplayName != nil {
				p.DisplayName = *u.DisplayName
			}
			if u.RequiredEntitlements != nil {
				p.RequiredEntitlements = *u.RequiredEntitlements
			}
			b.products[key], found = p, true
			out = append(out, p)
			break
		}
		if !found {
			return nil, fmt.Errorf("unknown product %s", u.ID)
		}
	}
	return out, nil
}

func TestChangedAccessPolicyWithdrawsExistingOffers(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	cfg := kinds.Config{Languages: []string{"en"}, Tiers: []string{"premium"}, Kinds: map[string]kinds.Kind{
		"gallery": {Route: "g", Presentation: "reader"},
	}}
	billing := &policyBilling{products: map[string]access.Product{}}
	gate, err := access.NewGate(access.GateConfig{Tenant: "policy", Kinds: cfg, Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg, Billing: billing, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	id, version := contentref.NewID(), contentref.NewID()
	for _, tenant := range []string{"policy", "other"} {
		exec(`INSERT INTO `+table("content_items")+` (tenant_id,id,kind) VALUES ($1,$2,'gallery')`, tenant, id)
		exec(`INSERT INTO `+table("content_versions")+` (tenant_id,id,content_id,languages) VALUES ($1,$2,$3,ARRAY['en'])`, tenant, version, id)
	}
	targets := []Target{{Kind: ContentTarget, ID: id}, {Kind: VersionTarget, ID: version, Content: id}}
	keys := []string{}
	for i, target := range targets {
		key, err := s.key(target)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key.String())
		product, price := fmt.Sprintf("product-%d", i), fmt.Sprintf("price-%d", i)
		billing.products[key.String()] = access.Product{ID: product, Key: key.String(), Prices: []access.Price{{ID: price}}}
		for _, tenant := range []string{"policy", "other"} {
			exec(`INSERT INTO `+table("content_offers")+` (tenant_id,id,target_kind,target_id,terms,revision,synced_revision,product_id,price_id)
 VALUES ($1,$2,$3,$4,'{"amount":499,"currency":"USD"}',1,1,$5,$6)`, tenant, contentref.NewID(), target.Kind, target.ID, product, price)
		}
	}
	if sales, err := s.Sales(ctx, keys); err != nil || len(sales) != 2 {
		t.Fatalf("unrestricted offers: %+v %v", sales, err)
	}
	k := cfg.Kinds["gallery"]
	k.AccessLevels = []string{"public", "tier"}
	cfg.Kinds["gallery"] = k
	if sales, err := s.Sales(ctx, keys); err != nil || len(sales) != 0 {
		t.Fatalf("restricted offers remain visible: %+v %v", sales, err)
	}

	riverSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	if s.client, err = river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{riverSchema, "river_job"}.Sanitize()+` WHERE kind='contentkit_offer_sync'`).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("already-synced offers queued=%d: %v", jobs, err)
	}
	if err := s.Sync(ctx, targets); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx, targets); err != nil {
		t.Fatalf("repeat sync: %v", err)
	}
	for _, key := range keys {
		if !billing.products[key].Archived {
			t.Fatalf("product remains purchasable: %s", key)
		}
	}
	var withdrawn, unchanged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table("content_offers")+` WHERE tenant_id='policy' AND state='withdrawn' AND revision=2 AND synced_revision=2 AND price_id IS NOT NULL`).Scan(&withdrawn); err != nil || withdrawn != 2 {
		t.Fatalf("withdrawal history count=%d: %v", withdrawn, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table("content_offers")+` WHERE tenant_id='other' AND state='active' AND revision=1 AND synced_revision=1`).Scan(&unchanged); err != nil || unchanged != 2 {
		t.Fatalf("other tenant unchanged count=%d: %v", unchanged, err)
	}
	// Enabling sales again does not accidentally reactivate archived history.
	k.AccessLevels = nil
	cfg.Kinds["gallery"] = k
	if sales, err := s.Sales(ctx, keys); err != nil || len(sales) != 0 {
		t.Fatalf("withdrawn offers reappeared: %+v %v", sales, err)
	}
}
