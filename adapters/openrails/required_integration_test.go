package openrails_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/offers"
)

// A group_ppv post's product sells only to its group's members: OpenRails
// refuses anyone else's order (entitlement_required, missing the group key the
// paywall requires) and takes a member's. Once the rule is plain PPV the
// requirement goes.
func TestGroupPPVRequiresMembershipIntegration(t *testing.T) {
	// Orders need provider writes; nothing here reaches a provider.
	s := newShop(t, func(c *openrails.Config) { c.ProviderWriteMode = openrails.ProviderWritesLimited })
	ctx := t.Context()
	owner, member, stranger := s.customer(), s.customer(), s.customer()
	studio := s.channel("studio", "Studio", owner.ID)
	none := []catalog.Creator{}
	post := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.GroupPPV, Group: studio}, OwnerGroupID: studio, Creators: &none,
		Text: map[string]catalog.Text{"en": {Title: "Members' PPV"}}})
	target := offers.Target{Kind: offers.ContentTarget, ID: post}
	s.sell(target, offers.Terms{Amount: 2_990_000})
	s.holds(member, gk(studio))
	product, prices := s.product(ck(post))
	if !slices.Equal(product.RequiredEntitlements, []string{gk(studio)}) || len(live(prices)) != 1 {
		t.Fatalf("product %+v requires %v", product.ID, product.RequiredEntitlements)
	}
	price := live(prices)[0].ID
	order := func(a access.Actor) error {
		t.Helper()
		customer, err := billing.ParseCustomerID(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.bill.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price}},
			IdempotencyKey: uuid.NewString()})
		return err
	}

	var refused *billing.StatusError
	if err := order(stranger); !errors.As(err, &refused) || refused.Code != billing.CodeEntitlementRequired {
		t.Fatalf("a stranger's order: %v", err)
	}
	if missing, _ := refused.Metadata["missing"].([]any); len(missing) != 1 || missing[0] != gk(studio) {
		t.Fatalf("the refusal misses %v", refused.Metadata)
	}
	var detail catalog.Content
	s.get(s.catalogAPI, stranger, "/contents/"+post, &detail)
	if detail.Decision.Requires != gk(studio) || len(detail.Decision.Offers) != 1 {
		t.Fatalf("the stranger's paywall %+v", detail.Decision)
	}
	if err := order(member); err != nil {
		t.Fatalf("a member's order: %v", err)
	}

	// Plain PPV: the next sync drops the requirement.
	if err := s.cat.Update(ctx, s.staff, []catalog.Update{{ID: post, ContentPatch: catalog.ContentPatch{Access: &access.Rule{Level: access.PPV}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.offers.Sync(ctx, []offers.Target{target}); err != nil {
		t.Fatal(err)
	}
	if product, _ = s.product(ck(post)); len(product.RequiredEntitlements) != 0 {
		t.Fatalf("a PPV product still requires %v", product.RequiredEntitlements)
	}
	if err := order(stranger); err != nil {
		t.Fatalf("a stranger's order for a PPV post: %v", err)
	}
}
