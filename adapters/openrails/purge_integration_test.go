package openrails_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/offers"
)

type failingArchive struct{ access.Billing }

func (b failingArchive) UpdateProducts(context.Context, []access.ProductUpdate) ([]access.Product, error) {
	return nil, errors.New("billing temporarily unavailable")
}

// Physical content deletion must not depend on a live billing service. The
// retained offer is enough to archive the original product later, even if its
// initial provider write committed before the local acknowledgement did.
func TestPurgeRetainsBillingHistoryIntegration(t *testing.T) {
	for _, version := range []bool{false, true} {
		for _, lostAck := range []bool{false, true} {
			name := map[bool]string{false: "content", true: "version"}[version]
			if lostAck {
				name += "-lost-ack"
			}
			t.Run(name, func(t *testing.T) {
				s := newShop(t, func(c *openrails.Config) { c.ProviderWriteMode = openrails.ProviderWritesLimited })
				ctx := t.Context()
				id := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, Text: map[string]catalog.Text{"en": {Title: "Purchased work"}}})
				target := offers.Target{Kind: offers.ContentTarget, ID: id}
				productKey := ck(id)
				if version {
					var vid string
					if err := s.pool.QueryRow(ctx, `SELECT id FROM `+s.table("content_versions")+` WHERE content_id=$1`, id).Scan(&vid); err != nil {
						t.Fatal(err)
					}
					target = offers.Target{Kind: offers.VersionTarget, ID: vid, Content: id}
					productKey = vk(id, vid)
				}
				s.sell(target, offers.Terms{Amount: 2_990_000})
				product, prices := s.product(productKey)
				buyer := s.customer()
				customer := must(billing.ParseCustomerID(buyer.ID))
				order, err := s.bill.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: prices[0].ID}}, IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
				grant, err := s.bill.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
				if err != nil {
					t.Fatal(err)
				}
				if lostAck {
					s.exec(`UPDATE ` + s.table("content_offers") + ` SET product_id=NULL,price_id=NULL,synced_revision=0`)
				}
				if version {
					err = s.cat.DeleteVersions(ctx, s.staff, []catalog.VersionKey{{ContentID: id, VersionID: target.ID}})
				} else {
					err = s.cat.DeleteForModeration(ctx, s.staff, []string{id})
				}
				if err != nil {
					t.Fatal(err)
				}
				// A failure after deletion must leave the archive obligation pending.
				unavailable, err := offers.New(offers.Options{Pool: s.pool, Schema: s.schema, Tenant: tenant, Kinds: s.kinds, Billing: failingArchive{s.billing}, Gate: s.gate})
				if err != nil {
					t.Fatal(err)
				}
				if err = unavailable.Sync(ctx, []offers.Target{target}); err == nil {
					t.Fatal("archive failure was ignored")
				}
				if n, err := s.cat.PurgeDeleted(ctx); err != nil || n != 0 {
					t.Fatalf("retention: n=%d err=%v", n, err)
				}
				s.exec(`UPDATE ` + s.table("content_items") + ` SET deleted_at=now()-interval '31 days' WHERE state='deleted'`)
				s.exec(`UPDATE ` + s.table("content_versions") + ` SET deleted_at=now()-interval '31 days' WHERE state='deleted'`)
				if n, err := s.cat.PurgeDeleted(ctx); err != nil || n != 1 {
					t.Fatalf("purge: n=%d err=%v", n, err)
				}
				var exists bool
				table := "content_items"
				if version {
					table = "content_versions"
				}
				if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table(table)+` WHERE id=$1)`, target.ID).Scan(&exists); err != nil || exists {
					t.Fatalf("source survives: %v %v", exists, err)
				}
				retryTarget := target
				retryTarget.Content = "" // the reconciler only knows kind and ID
				for range 2 {
					if err = s.offers.Sync(ctx, []offers.Target{retryTarget}); err != nil {
						t.Fatal(err)
					}
				}
				after, afterPrices := s.product(productKey)
				if !after.Archived || after.ID != product.ID || after.DisplayName != product.DisplayName || !reflect.DeepEqual(prices, afterPrices) {
					t.Fatalf("history changed: product=%+v prices=%+v", after, afterPrices)
				}
				kept, err := s.bill.GetOrder(ctx, order.ID)
				if err != nil || !reflect.DeepEqual(kept.Lines, order.Lines) || kept.Total != order.Total {
					t.Fatalf("order changed: %+v %v", kept, err)
				}
				grants, err := s.bill.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product.ID}})
				if err != nil || len(grants.Items) != 1 || grants.Items[0].ID != grant.Items[0].ID || !s.holdsNow(buyer, productKey) {
					t.Fatalf("purchase access lost: %+v %v", grants, err)
				}
				live, err := s.offers.Live(ctx, []access.Key{key(productKey)})
				if err != nil || len(live) != 0 {
					t.Fatalf("purged content still sold: %+v %v", live, err)
				}
			})
		}
	}
}
