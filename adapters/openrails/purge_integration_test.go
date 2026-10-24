package openrails_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
)

// Content removal never edits the billing catalog or destroys historical
// purchases. Product archival, if wanted, is an explicit OpenRails operation.
func TestPurgeRetainsBillingHistoryIntegration(t *testing.T) {
	for _, version := range []bool{false, true} {
		t.Run(map[bool]string{false: "content", true: "version"}[version], func(t *testing.T) {
			s := newShop(t, func(c *openrails.Config) { c.ProviderWriteMode = openrails.ProviderWritesLimited })
			ctx := t.Context()
			id := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, Text: map[string]catalog.Text{"en": {Title: "Purchased work"}}})
			targetID, productKey := id, ck(id)
			if version {
				if err := s.pool.QueryRow(ctx, `SELECT id FROM `+s.table("content_versions")+` WHERE content_id=$1`, id).Scan(&targetID); err != nil {
					t.Fatal(err)
				}
				productKey = vk(id, targetID)
			}
			_, price := s.sell(productKey, "Billing-owned purchased work", 2_990_000)
			product, prices := s.product(productKey)
			buyer := s.customer()
			customer := must(billing.ParseCustomerID(buyer.ID))
			order, err := s.bill.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			payment, err := s.bill.RecordExternalPayment(ctx, billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: order.Total, TransactionID: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			order, err = s.bill.GetOrder(ctx, order.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeAccess, err := s.bill.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if version {
				err = s.cat.DeleteVersions(ctx, s.staff, []catalog.VersionKey{{ContentID: id, VersionID: targetID}})
			} else {
				err = s.cat.DeleteForModeration(ctx, s.staff, []string{id})
			}
			if err != nil {
				t.Fatal(err)
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
			if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table(table)+` WHERE id=$1)`, targetID).Scan(&exists); err != nil || exists {
				t.Fatalf("source survives: %v %v", exists, err)
			}
			after, afterPrices := s.product(productKey)
			if !reflect.DeepEqual(after, product) || !reflect.DeepEqual(afterPrices, prices) {
				t.Fatalf("billing catalog changed: product=%+v prices=%+v", after, afterPrices)
			}
			kept, err := s.bill.GetOrder(ctx, order.ID)
			if err != nil || !reflect.DeepEqual(kept.Lines, order.Lines) || kept.Total != order.Total {
				t.Fatalf("order changed: %+v %v", kept, err)
			}
			grants, err := s.bill.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product.ID}})
			if err != nil || len(grants.Items) != len(beforeAccess.Items) || !s.holdsNow(buyer, productKey) {
				t.Fatalf("purchase access lost: %+v %v", grants, err)
			}
			if !slices.ContainsFunc(grants.Items, func(g billing.ProductAccessGrant) bool {
				return g.SourceType == billing.ProductAccessSourcePurchase && g.PaymentID != nil && *g.PaymentID == payment.ID
			}) {
				t.Fatal("purchased access window lost")
			}
			keptPayment, err := s.bill.GetPayment(ctx, payment.ID)
			if err != nil || keptPayment.ID != payment.ID || keptPayment.Status != billing.PaymentSucceeded || keptPayment.Amount != payment.Amount || keptPayment.Currency != payment.Currency || keptPayment.TransactionID != payment.TransactionID || keptPayment.CustomerID != payment.CustomerID || !reflect.DeepEqual(keptPayment.OrderID, payment.OrderID) {
				t.Fatalf("payment changed: %+v %v", keptPayment, err)
			}
		})
	}
}
