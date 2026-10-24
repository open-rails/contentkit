package openrails_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
)

// Paywalls return stable access keys. Prices are read and managed by the host
// through OpenRails, without a second ContentKit price catalog.
func TestPaywallEntitlementsIntegration(t *testing.T) {
	s := newShop(t)
	buyer, stranger := s.customer(), s.customer()
	ppv := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, Creators: &[]catalog.Creator{}, Text: map[string]catalog.Text{"en": {Title: "Night Train"}}})
	product, price := s.sell(ck(ppv), "Night Train purchase", 2_990_000)
	var detail catalog.Content
	raw := s.get(s.catalogAPI, stranger, "/contents/"+ppv, &detail)
	if detail.Decision.Allowed || !slices.Contains(detail.Decision.Sell, ck(ppv)) {
		t.Fatalf("missing entitlement requirement: %+v", detail.Decision)
	}
	if containsBillingTerms(raw) {
		t.Fatalf("ContentKit response contains a duplicate sale: %s", raw)
	}
	var page discovery.ContentPage
	raw = s.get(s.discoveryAPI, stranger, "/contents?kind=post", &page)
	if len(page.Contents) != 1 || page.Contents[0].Access.Allowed || !slices.Contains(page.Contents[0].Access.Sell, ck(ppv)) || containsBillingTerms(raw) {
		t.Fatalf("card access: %+v", page)
	}
	s.holds(buyer, ck(ppv))
	detail = catalog.Content{}
	s.get(s.catalogAPI, buyer, "/contents/"+ppv, &detail)
	if !detail.Decision.Allowed {
		t.Fatalf("OpenRails grant did not unlock content: %+v", detail.Decision)
	}
	after, prices := s.product(ck(ppv))
	if after.ID != product.ID || len(prices) != 1 || prices[0].ID != price.ID || prices[0].UnitAmount != price.UnitAmount {
		t.Fatal("content reads modified OpenRails product or price")
	}
}

func containsBillingTerms(raw string) bool {
	return strings.Contains(raw, `"offers"`) || strings.Contains(raw, `"offer"`) || strings.Contains(raw, `"price_id"`) || strings.Contains(raw, `"amount"`)
}
