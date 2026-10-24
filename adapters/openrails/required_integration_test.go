package openrails_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	billingcatalog "github.com/open-rails/openrails/catalog"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
)

// Both services enforce their own boundary: ContentKit requires membership
// for reading, and the host sets OpenRails' prerequisite for purchasing.
func TestEntityPPVRequiresMembershipIntegration(t *testing.T) {
	s := newShop(t, func(c *openrails.Config) { c.ProviderWriteMode = openrails.ProviderWritesLimited })
	ctx := t.Context()
	owner, member, stranger := s.customer(), s.customer(), s.customer()
	studio := s.channel("studio", "Studio", owner.ID)
	post := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.EntityPPV, Entity: studio}, OwnerEntityID: studio, Creators: &[]catalog.Creator{}, Text: map[string]catalog.Text{"en": {Title: "Members' PPV"}}})
	product, price := s.sell(ck(post), "Members purchase", 2_990_000)
	if _, err := s.bill.UpdateProduct(ctx, product.ID, billing.UpdateProductParams{RequiredEntitlements: billingcatalog.Value([]string{gk(studio)})}); err != nil {
		t.Fatal(err)
	}
	s.holds(member, gk(studio))
	order := func(a access.Actor) error {
		t.Helper()
		_, err := s.bill.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: must(billing.ParseCustomerID(a.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: uuid.NewString()})
		return err
	}
	var refused *billing.StatusError
	if err := order(stranger); !errors.As(err, &refused) || refused.Code != billing.CodeEntitlementRequired {
		t.Fatalf("stranger order: %v", err)
	}
	if missing, _ := refused.Metadata["missing"].([]any); len(missing) != 1 || missing[0] != gk(studio) {
		t.Fatalf("missing prerequisite: %v", refused.Metadata)
	}
	var detail catalog.Content
	s.get(s.catalogAPI, stranger, "/contents/"+post, &detail)
	if detail.Decision.Requires != gk(studio) || !slices.Contains(detail.Decision.Sell, ck(post)) {
		t.Fatalf("stranger paywall: %+v", detail.Decision)
	}
	if err := order(member); err != nil {
		t.Fatalf("member order: %v", err)
	}

	// Content edits cannot silently change the merchant's purchase conditions.
	if err := s.cat.Update(ctx, s.staff, []catalog.Update{{ID: post, ContentPatch: catalog.ContentPatch{Access: &access.Rule{Level: access.PPV}}}}); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := s.product(ck(post))
	if !slices.Equal(unchanged.RequiredEntitlements, []string{gk(studio)}) {
		t.Fatalf("content edit changed billing prerequisites: %+v", unchanged)
	}
	if _, err := s.bill.UpdateProduct(ctx, product.ID, billing.UpdateProductParams{RequiredEntitlements: billingcatalog.Value([]string{})}); err != nil {
		t.Fatal(err)
	}
	if err := order(stranger); err != nil {
		t.Fatalf("explicit OpenRails change did not clear prerequisite: %v", err)
	}
}
