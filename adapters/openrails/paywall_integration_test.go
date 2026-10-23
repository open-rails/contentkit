package openrails_test

import (
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/offers"
)

// Paywalls carry the purchasable offer of each key they sell, on detail,
// version decisions and cards, priced as OpenRails sells them; a tier key and
// terms not yet synced carry none. The item's offer editors (creators, its
// channel's owner, staff) see each current offer.
func TestPaywallOffersIntegration(t *testing.T) {
	s := newShop(t)
	ctx := t.Context()
	owner, creator, stranger := s.customer(), s.customer(), s.customer()
	studio := s.channel("studio", "Studio", owner.ID)
	month := 720
	s.sell(offers.Target{Kind: offers.EntityTarget, ID: studio}, offers.Terms{Amount: 4_990_000, RenewHours: &month})
	credits := []catalog.Creator{{UserID: creator.ID}}
	none := []catalog.Creator{}
	ppv := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, Creators: &credits,
		Text:     map[string]catalog.Text{"en": {Title: "Night Train"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"ja"}}}})
	members := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.EntityLevel, Entity: studio}, OwnerEntityID: studio, Creators: &none,
		Text: map[string]catalog.Text{"en": {Title: "Backstage"}}})
	owned := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.PPV}, OwnerEntityID: studio, Creators: &none,
		Text: map[string]catalog.Text{"en": {Title: "Owned"}}})
	premium := s.post(catalog.ContentInput{Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}, Creators: &none,
		Text: map[string]catalog.Text{"en": {Title: "Premium"}}})
	read := func(as access.Actor, id string) catalog.Content {
		t.Helper()
		var c catalog.Content
		s.get(s.catalogAPI, as, "/contents/"+id, &c)
		return c
	}
	ja := read(s.staff, ppv).Versions[1].ID

	s.sell(offers.Target{Kind: offers.ContentTarget, ID: ppv}, offers.Terms{Amount: 2_990_000, AccessHours: &month})
	s.sell(offers.Target{Kind: offers.VersionTarget, ID: ja, Content: ppv}, offers.Terms{Amount: 990_000})
	s.sell(offers.Target{Kind: offers.ContentTarget, ID: owned}, offers.Terms{Amount: 1_990_000})
	priceOf := func(key string) string {
		_, prices := s.product(key)
		if live := live(prices); len(live) == 1 {
			return string(live[0].ID)
		}
		t.Fatalf("%s has no one live price", key)
		return ""
	}
	sale := func(key string, kind offers.TargetKind, id, content string, amount offers.Amount, hours, renew *int) offers.Sale {
		return offers.Sale{Key: must(access.ParseKey(key)), Target: offers.Target{Kind: kind, ID: id, Content: content},
			Terms: offers.Terms{Amount: amount, Currency: "USD", AccessHours: hours, RenewHours: renew}, PriceID: priceOf(key)}
	}
	contentSale := sale(ck(ppv), offers.ContentTarget, ppv, "", 2_990_000, &month, nil)
	versionSale := sale(vk(ppv, ja), offers.VersionTarget, ja, ppv, 990_000, nil, nil)
	groupSale := sale(gk(studio), offers.EntityTarget, studio, "", 4_990_000, nil, &month)
	same := func(name string, got, want []offers.Sale) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: offers %+v, want %+v", name, got, want)
			return
		}
		for i := range got {
			g, w := got[i], want[i]
			if g.Key != w.Key || g.Target != w.Target || g.PriceID != w.PriceID || !sameTerms(g.Terms, w.Terms) {
				t.Errorf("%s: offer %d %+v, want %+v", name, i, g, w)
			}
		}
	}

	detail := read(stranger, ppv)
	same("detail", detail.Decision.Offers, []offers.Sale{contentSale})
	same("the English version", detail.Versions[0].Decision.Offers, []offers.Sale{contentSale})
	same("the Japanese version", detail.Versions[1].Decision.Offers, []offers.Sale{versionSale, contentSale})
	if detail.Offer != nil || detail.Versions[1].Offer != nil {
		t.Fatalf("a stranger sees the editors' offers %+v %+v", detail.Offer, detail.Versions[1].Offer)
	}
	if detail = read(stranger, owned); detail.Offer != nil || detail.Editor {
		t.Fatalf("a stranger edits the channel's post %+v", detail.Offer)
	}
	detail = read(stranger, members)
	same("a members post", detail.Decision.Offers, []offers.Sale{groupSale})
	detail = read(stranger, premium)
	if detail.Decision.Allowed || len(detail.Decision.Sell) != 3 || len(detail.Decision.Offers) != 0 {
		t.Fatalf("a tier post's paywall %+v", detail.Decision)
	}

	var page discovery.ContentPage
	s.get(s.discoveryAPI, stranger, "/contents?kind=post", &page)
	cards := map[string]discovery.CardAccess{}
	for _, c := range page.Contents {
		cards[c.ID] = c.Access
	}
	same("the PPV card", cards[ppv].Offers, []offers.Sale{contentSale})
	same("the members card", cards[members].Offers, []offers.Sale{groupSale})
	same("the premium card", cards[premium].Offers, nil)

	// Offer editors see the current offers: the creator, the channel's owner,
	// staff.
	for _, c := range []struct {
		name   string
		viewer access.Actor
		id     string
	}{{"the creator", creator, ppv}, {"staff", s.staff, ppv}, {"the channel owner", owner, owned}} {
		detail = read(c.viewer, c.id)
		if o := detail.Offer; !detail.Editor || o == nil || o.State != offers.Active || !o.Synced || o.PriceID == "" {
			t.Errorf("%s: editor %v, the item's offer %+v", c.name, detail.Editor, o)
		}
	}
	detail = read(creator, ppv)
	if o := detail.Versions[1].Offer; o == nil || o.Target.ID != ja || o.PriceID != versionSale.PriceID || detail.Versions[0].Offer != nil {
		t.Fatalf("the creator's version offers %+v %+v", detail.Versions[0].Offer, o)
	}
	if detail.Offer.ID == "" || detail.Offer.Terms.Amount != 2_990_000 {
		t.Fatalf("the creator's offer %+v", detail.Offer)
	}

	// A reprice sells nothing until OpenRails sells it.
	target := offers.Target{Kind: offers.ContentTarget, ID: ppv}
	if _, err := s.offers.Put(ctx, []offers.Put{{Target: target, Terms: offers.Terms{Amount: 3_990_000}}}); err != nil {
		t.Fatal(err)
	}
	detail = read(stranger, ppv)
	same("an unsynced reprice", detail.Decision.Offers, nil)
	detail = read(creator, ppv)
	if o := detail.Offer; o == nil || o.Synced || o.Terms.Amount != 3_990_000 {
		t.Fatalf("the creator's unsynced offer %+v", o)
	}
	if err := s.offers.Sync(ctx, []offers.Target{target}); err != nil {
		t.Fatal(err)
	}
	detail = read(stranger, ppv)
	same("a synced reprice", detail.Decision.Offers, []offers.Sale{sale(ck(ppv), offers.ContentTarget, ppv, "", 3_990_000, nil, nil)})
}

func sameTerms(a, b offers.Terms) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Amount == b.Amount && a.Currency == b.Currency && eq(a.AccessHours, b.AccessHours) && eq(a.RenewHours, b.RenewHours)
}
