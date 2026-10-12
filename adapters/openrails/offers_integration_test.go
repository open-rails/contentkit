package openrails_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/offers"
	riverhelpers "github.com/open-rails/helpers/river"
)

// product reads the OpenRails product of key with every price it ever had.
func (w *world) product(key string) (billing.Product, []billing.Price) {
	w.t.Helper()
	ctx := w.t.Context()
	page, err := w.bill.ListProducts(ctx, billing.ProductListParams{Keys: []string{key}})
	if err != nil || len(page.Items) != 1 {
		w.t.Fatalf("product %s: %v %v", key, page, err)
	}
	p := page.Items[0]
	prices, err := w.bill.ListPrices(ctx, billing.PriceListParams{ProductID: p.ID})
	if err != nil {
		w.t.Fatal(err)
	}
	return p, prices.Items
}

func (w *world) offer(t offers.Target) offers.Offer {
	w.t.Helper()
	page, err := w.offers.List(w.t.Context(), offers.ListQuery{Limit: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, o := range page.Offers {
		if o.Target.Kind == t.Kind && o.Target.ID == t.ID && o.State == offers.Active {
			return o
		}
	}
	w.t.Fatalf("no active offer on %+v", t)
	return offers.Offer{}
}

func live(prices []billing.Price) []billing.Price {
	return slices.DeleteFunc(slices.Clone(prices), func(p billing.Price) bool { return p.Archived })
}

// An offer's life against a real OpenRails: the product's key and only
// entitlement are the access key and its name follows the title; a retried
// sync changes nothing; a reprice is a new price archiving the old; a
// rename renames; a withdrawal archives the product while buyers keep their
// key; selling again restores it.
func TestOfferSyncLifecycle(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.content(access.Rule{Level: access.PPV}, "Night Train", "")
	target := offers.Target{Kind: offers.ContentTarget, ID: c}
	sell := func(amount offers.Amount) offers.Offer {
		t.Helper()
		out, err := w.offers.Put(ctx, []offers.Put{{Target: target, Terms: offers.Terms{Amount: amount}}})
		if err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	sync := func() offers.Offer {
		t.Helper()
		if err := w.offers.Sync(ctx, []offers.Target{target}); err != nil {
			t.Fatal(err)
		}
		return w.offer(target)
	}

	o := sell(4_990_000)
	if o.Synced || o.Key.String() != ck(c) || o.Terms.Currency != "USD" || o.Revision != 1 {
		t.Fatalf("put %+v", o)
	}
	var everSold bool
	if err := w.pool.QueryRow(ctx, `SELECT ever_sold FROM `+w.table("content_items")+` WHERE id = $1`, c).Scan(&everSold); err != nil || !everSold {
		t.Fatalf("ever_sold %v %v", everSold, err)
	}
	o = sync()
	p, prices := w.product(ck(c))
	if !o.Synced || o.ProductID != p.ID.String() || p.Key != ck(c) || !slices.Equal(p.Entitlements, []string{ck(c)}) ||
		p.DisplayName != "Night Train" || p.Archived || len(prices) != 1 || prices[0].UnitAmount != 4_990_000 ||
		o.PriceID != prices[0].ID.String() || prices[0].Key != offers.PriceKey {
		t.Fatalf("synced %+v product %+v prices %+v", o, p, prices)
	}
	if again := sell(4_990_000); again.Revision != 1 {
		t.Fatalf("equal terms moved the revision: %+v", again)
	}

	// A retry after a lost acknowledgement creates nothing.
	w.exec(`UPDATE ` + w.table("content_offers") + ` SET synced_revision = 0`)
	if again := sync(); again.PriceID != o.PriceID || again.ProductID != o.ProductID {
		t.Fatalf("retry %+v", again)
	}
	if _, prices := w.product(ck(c)); len(prices) != 1 {
		t.Fatalf("retry made %d prices", len(prices))
	}

	// Reprice: a new price; the old one is archived.
	sell(5_990_000)
	repriced := sync()
	_, prices = w.product(ck(c))
	if lv := live(prices); repriced.PriceID == o.PriceID || repriced.Revision != 2 || len(prices) != 2 || len(lv) != 1 || lv[0].ID.String() != repriced.PriceID || lv[0].UnitAmount != 5_990_000 {
		t.Fatalf("reprice %+v prices %+v", repriced, prices)
	}
	m, err := w.offers.Live(ctx, []access.Key{key(ck(c)), key("premium")})
	if err != nil || len(m) != 1 || m[ck(c)].PriceID != repriced.PriceID {
		t.Fatalf("Live %v %v", m, err)
	}

	// A rename follows the title.
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE `+w.table("content_text")+` SET title = 'Night Train II' WHERE content_id = $1`, c); err != nil {
			return err
		}
		return w.offers.Resync(ctx, tx, []offers.Target{target})
	}); err != nil {
		t.Fatal(err)
	}
	sync()
	if p, _ := w.product(ck(c)); p.DisplayName != "Night Train II" {
		t.Fatalf("name %q", p.DisplayName)
	}

	// Withdrawal archives the product; a buyer keeps the key.
	buyer := w.customer()
	w.holds(buyer, ck(c))
	if _, err := w.offers.Withdraw(ctx, []offers.Target{target}); err != nil {
		t.Fatal(err)
	}
	if err := w.offers.Sync(ctx, []offers.Target{target}); err != nil {
		t.Fatal(err)
	}
	if p, _ := w.product(ck(c)); !p.Archived || !w.holdsNow(buyer, ck(c)) {
		t.Fatalf("withdrawn: archived %v, buyer holds %v", p.Archived, w.holdsNow(buyer, ck(c)))
	}
	if _, err := w.offers.Withdraw(ctx, []offers.Target{target}); !errors.Is(err, offers.ErrNotFound) {
		t.Fatalf("withdraw twice: %v", err)
	}

	// Selling again restores the product with the new terms.
	again := sell(5_990_000)
	if again.ID == o.ID {
		t.Fatal("a new offer reuses the withdrawn one")
	}
	if again = sync(); again.PriceID != repriced.PriceID {
		t.Fatalf("the same terms are the same price: %+v", again)
	}
	if p, _ := w.product(ck(c)); p.Archived {
		t.Fatal("product still archived")
	}
}

// Versions and groups sell their own keys; terms are checked per target.
func TestOfferTargets(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.content(access.Rule{Level: access.PPV}, "Atlas", "")
	v := w.version(c, nil, "Director's cut")
	other := w.content(access.Rule{Level: access.PPV}, "Other", "")
	channel, tag := w.group("channel", "Night Owls", ""), w.group("tag", "Noir", "")
	monthly := 720

	out, err := w.offers.Put(ctx, []offers.Put{
		{Target: offers.Target{Kind: offers.VersionTarget, ID: v, Content: c}, Terms: offers.Terms{Amount: 1_000_000}},
		{Target: offers.Target{Kind: offers.GroupTarget, ID: channel}, Terms: offers.Terms{Amount: 4_000_000, RenewHours: &monthly}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.offers.Sync(ctx, []offers.Target{out[0].Target, out[1].Target}); err != nil {
		t.Fatal(err)
	}
	if p, prices := w.product(vk(c, v)); p.DisplayName != "Atlas (Director's cut)" || len(prices) != 1 || prices[0].BillingIntervalHours != nil {
		t.Fatalf("version product %+v %+v", p, prices)
	}
	if p, prices := w.product(gk(channel)); p.DisplayName != "Night Owls" || len(prices) != 1 || prices[0].BillingIntervalHours == nil || *prices[0].BillingIntervalHours != monthly {
		t.Fatalf("group product %+v %+v", p, prices)
	}
	var sold bool
	if err := w.pool.QueryRow(ctx, `SELECT ever_sold FROM `+w.table("content_groups")+` WHERE id = $1`, channel).Scan(&sold); err != nil || !sold {
		t.Fatalf("group ever_sold %v %v", sold, err)
	}
	// Free again: no price is on sale and the sync stops every renewal.
	if _, err := w.offers.Put(ctx, []offers.Put{{Target: offers.Target{Kind: offers.GroupTarget, ID: channel}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.offers.Sync(ctx, []offers.Target{out[1].Target}); err != nil {
		t.Fatalf("a renewing group's offer made free: %v", err)
	}
	if p, prices := w.product(gk(channel)); p.Archived || slices.ContainsFunc(prices, func(pr billing.Price) bool { return !pr.Archived }) {
		t.Fatalf("free group product %+v %+v", p, prices)
	}

	for name, tc := range map[string]struct {
		put  offers.Put
		want error
	}{
		"below minimum":     {offers.Put{Target: offers.Target{Kind: offers.ContentTarget, ID: c}, Terms: offers.Terms{Amount: 100_000}}, offers.ErrInvalid},
		"free content":      {offers.Put{Target: offers.Target{Kind: offers.ContentTarget, ID: c}}, offers.ErrInvalid},
		"renewing content":  {offers.Put{Target: offers.Target{Kind: offers.ContentTarget, ID: c}, Terms: offers.Terms{Amount: 1_000_000, RenewHours: &monthly}}, offers.ErrInvalid},
		"bad currency":      {offers.Put{Target: offers.Target{Kind: offers.ContentTarget, ID: c}, Terms: offers.Terms{Amount: 1_000_000, Currency: "usd"}}, offers.ErrInvalid},
		"over maximum":      {offers.Put{Target: offers.Target{Kind: offers.ContentTarget, ID: c}, Terms: offers.Terms{Amount: offers.MaxAmount + 1}}, offers.ErrInvalid},
		"version elsewhere": {offers.Put{Target: offers.Target{Kind: offers.VersionTarget, ID: v, Content: other}, Terms: offers.Terms{Amount: 1_000_000}}, offers.ErrNotFound},
		"unknown content":   {offers.Put{Target: offers.Target{Kind: offers.ContentTarget, ID: uid(77)}, Terms: offers.Terms{Amount: 1_000_000}}, offers.ErrNotFound},
		"unsellable group":  {offers.Put{Target: offers.Target{Kind: offers.GroupTarget, ID: tag}, Terms: offers.Terms{Amount: 1_000_000}}, offers.ErrConflict},
		"group below min":   {offers.Put{Target: offers.Target{Kind: offers.GroupTarget, ID: channel}, Terms: offers.Terms{Amount: 500_000}}, offers.ErrInvalid},
	} {
		if _, err := w.offers.Put(ctx, []offers.Put{tc.put}); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

// Through an engine River client composed from RiverJobs, a Put queues its
// sync in the writing transaction and the worker brings OpenRails in line;
// the reconcile queues what was written while River was not bound.
func TestOfferSyncRiverJob(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	unbound := w.content(access.Rule{Level: access.PPV}, "Before River", "")
	if _, err := w.offers.Put(ctx, []offers.Put{{Target: offers.Target{Kind: offers.ContentTarget, ID: unbound}, Terms: offers.Terms{Amount: 1_000_000}}}); err != nil {
		t.Fatal(err)
	}
	riverSchema := "ckr_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if err := riverhelpers.ApplyMigrations(ctx, w.pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+pgx.Identifier{riverSchema}.Sanitize()+` CASCADE`)
	})
	client, err := riverhelpers.New(ctx, w.pool, &river.Config{Schema: riverSchema, FetchPollInterval: 100 * time.Millisecond}, w.offers.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stop)
	})

	c := w.content(access.Rule{Level: access.PPV}, "By River", "")
	if _, err := w.offers.Put(ctx, []offers.Put{{Target: offers.Target{Kind: offers.ContentTarget, ID: c}, Terms: offers.Terms{Amount: 2_000_000}}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{c, unbound} {
		target := offers.Target{Kind: offers.ContentTarget, ID: id}
		deadline := time.Now().Add(30 * time.Second)
		for !w.offer(target).Synced {
			if time.Now().After(deadline) {
				t.Fatalf("%s never synced: %+v", id, w.offer(target))
			}
			time.Sleep(100 * time.Millisecond)
		}
		if p, prices := w.product(ck(id)); p.Archived || len(live(prices)) != 1 {
			t.Fatalf("%s: product %+v prices %+v", id, p, prices)
		}
	}
}
