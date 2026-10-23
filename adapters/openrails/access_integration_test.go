package openrails_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/offers"
)

var errGrantResponse = errors.New("grant response lost")

// The real server commits normally; only its response or a later request fails.
type grantFaultClient struct {
	ckopenrails.Client
	calls, failAt int
	commit        bool
	before        func() error
}

func (c *grantFaultClient) CreateProductAccess(ctx context.Context, p billing.CreateProductAccessBatchParams, opts ...openrails.RequestOption) ([]billing.ProductAccessGrant, error) {
	c.calls++
	if c.before != nil {
		before := c.before
		c.before = nil
		if err := before(); err != nil {
			return nil, err
		}
	}
	if c.calls == c.failAt && !c.commit {
		return nil, errGrantResponse
	}
	out, err := c.Client.CreateProductAccess(ctx, p, opts...)
	if err == nil && c.calls == c.failAt {
		return nil, errGrantResponse
	}
	return out, err
}

func TestGrantReceiptsPreserveIndependentAccess(t *testing.T) {
	for _, tc := range []struct {
		name          string
		count, failAt int
		commit        bool
	}{
		{"lost response", 1, 1, true},
		{"later batch not sent", billing.MaxBatchItems + 1, 2, false},
		{"later batch response lost", billing.MaxBatchItems + 1, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			ctx := t.Context()
			viewer := w.customer()
			product, err := w.bill.CreateProduct(ctx, billing.CreateProductParams{Key: "receipt-test", DisplayName: "Receipt test", Entitlements: []string{"receipt-test"}})
			if err != nil {
				t.Fatal(err)
			}
			grant := access.Grant{Subject: viewer.ID, ProductID: product.ID.String()}
			independent, err := w.billing.Grant(ctx, contentref.NewID(), []access.Grant{grant})
			if err != nil {
				t.Fatal(err)
			}
			fault := &grantFaultClient{Client: w.bill, failAt: tc.failAt, commit: tc.commit}
			adapter := ckopenrails.Billing(fault)
			grants := make([]access.Grant, tc.count)
			for i := range grants {
				grants[i] = grant
			}
			key := contentref.NewID()
			prefix, err := adapter.Grant(ctx, key, grants)
			if !errors.Is(err, errGrantResponse) || len(prefix) != (tc.failAt-1)*billing.MaxBatchItems {
				t.Fatalf("failed batch: %d known IDs, %v", len(prefix), err)
			}
			ids, err := adapter.Grant(ctx, key, grants)
			if err != nil || len(ids) != tc.count || !slices.Equal(prefix, ids[:len(prefix)]) {
				t.Fatalf("recovered IDs: %v, %v", ids, err)
			}
			again, err := adapter.Grant(ctx, key, grants)
			if err != nil || !slices.Equal(ids, again) {
				t.Fatalf("replayed IDs changed: %v, %v", again, err)
			}
			for range 2 {
				if err := adapter.RevokeGrants(ctx, ids); err != nil {
					t.Fatal(err)
				}
			}
			customer, _ := billing.ParseCustomerID(viewer.ID)
			live, err := w.bill.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product.ID}, LiveOnly: true})
			if err != nil || len(live.Items) != 1 || live.Items[0].ID.String() != independent[0] {
				t.Fatalf("compensation must leave only the independent grant: %+v, %v", live, err)
			}
			if !w.holdsNow(viewer, "receipt-test") {
				t.Fatal("independently granted access was removed")
			}
		})
	}
}

func TestJoinCompensationPreservesConcurrentMembership(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	owner, viewer := w.customer(), w.customer()
	a, b := w.group("channel", "A", owner.ID), w.group("channel", "B", owner.ID)
	targets := []offers.Target{{Kind: offers.GroupTarget, ID: a}, {Kind: offers.GroupTarget, ID: b}}
	if _, err := w.offers.Put(ctx, []offers.Put{{Target: targets[0]}, {Target: targets[1]}}); err != nil {
		t.Fatal(err)
	}
	if err := w.offers.Sync(ctx, targets); err != nil {
		t.Fatal(err)
	}
	fault := &grantFaultClient{Client: w.bill, failAt: 1, commit: true, before: func() error {
		if _, err := w.offers.Join(ctx, viewer, []string{a}); err != nil {
			return err
		}
		_, err := w.offers.Withdraw(ctx, targets[1:])
		return err
	}}
	svc, err := offers.New(offers.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: w.kinds, Billing: ckopenrails.Billing(fault), Gate: w.gate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Join(ctx, viewer, []string{a, b}); !errors.Is(err, errGrantResponse) || !errors.Is(err, offers.ErrNotJoinable) {
		t.Fatalf("failed batch: %v", err)
	}
	if !w.holdsNow(viewer, gk(a)) || w.holdsNow(viewer, gk(b)) {
		t.Fatal("failed batch removed the successful membership or retained the withdrawn one")
	}
}

// Rules stored on content and versions decide each version against keys a
// real OpenRails holds: a version opens with its version key, its content's
// key, the key of a group holding it, a tier its rule names (also from a
// token claim, without a billing read), the key of its rule's group, or a
// free rule; a locked one offers what opens it, its preview and when it
// becomes free.
func TestDecideStoredRules(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	past, future := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(24*time.Hour).UTC().Truncate(time.Second)
	channel, series := w.group("channel", "Studio", ""), w.group("series", "Saga", "")

	book := w.content(access.Rule{Level: access.PPV, Preview: &access.Preview{Pages: 3}}, "Book", "")
	bookEN := w.version(book, nil, "English")
	bookUncut := w.version(book, &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}, "Uncut")
	saga := w.content(access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}, "Saga 1", "")
	sagaV := w.version(saga, nil, "")
	w.member(series, saga)
	members := w.content(access.Rule{Level: access.GroupLevel, Group: channel}, "Members only", "")
	membersV := w.version(members, nil, "")
	mppv := w.content(access.Rule{Level: access.GroupPPV, Group: channel}, "Members PPV", "")
	mppvV := w.version(mppv, nil, "")
	aged := w.content(access.Rule{Level: access.PPV, Releases: []access.Release{{At: past, Level: access.Public}}}, "Aged", "")
	agedV := w.version(aged, nil, "")
	fresh := w.content(access.Rule{Level: access.PPV, Releases: []access.Release{{At: future, Level: access.Public}}}, "Fresh", "")
	freshV := w.version(fresh, nil, "")
	free := w.content(access.Rule{Level: access.Public}, "Free", "")
	freeV := w.version(free, nil, "")

	page := w.items(bookEN, bookUncut, sagaV, membersV, mppvV, agedV, freshV, freeV)
	contentBuyer, versionBuyer, bundle, premium, member, nobody := w.customer(), w.customer(), w.customer(), w.customer(), w.customer(), w.customer()
	w.holds(contentBuyer, ck(book))
	w.holds(versionBuyer, vk(book, bookEN), ck(mppv))
	w.holds(bundle, gk(series))
	w.holds(premium, "premium")
	w.holds(member, gk(channel))
	claimed := w.customer()
	claimed.Verified = claims{id: claimed.ID, ents: []string{"premium"}}
	for name, tc := range map[string]struct {
		viewer access.Actor
		open   string // per page item: 1 opens
	}{
		"anonymous":        {access.Actor{Anonymous: true}, "00000101"},
		"nobody":           {nobody, "00000101"},
		"content key":      {contentBuyer, "11000101"},
		"version key":      {versionBuyer, "10001101"},
		"series bundle":    {bundle, "00100101"},
		"premium":          {premium, "01000101"},
		"premium by claim": {claimed, "01000101"},
		"channel member":   {member, "00010101"},
	} {
		w.client.take()
		mctx := access.WithMemo(ctx)
		ds, err := w.gate.Decide(mctx, tc.viewer, page)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, d := range ds {
			got += map[bool]string{true: "1", false: "0"}[d.Allowed]
		}
		if got != tc.open {
			t.Errorf("%s: opens %s, want %s", name, got, tc.open)
		}
		if reads, want := w.client.take(), map[bool]int{true: 0, false: 1}[tc.viewer.Anonymous]; reads != want {
			t.Errorf("%s: %d billing reads, want %d", name, reads, want)
		}
	}
	// A claimed tier needs no read at all.
	if ds, err := w.gate.Decide(ctx, claimed, w.items(bookUncut)); err != nil || !ds[0].Allowed || w.client.take() != 0 {
		t.Fatalf("claimed premium: %+v %v", ds, err)
	}

	// What a locked version offers.
	ds, err := w.gate.Decide(ctx, nobody, page)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		sell     []string
		requires string
		preview  *access.Preview
		after    *time.Time
	}{
		{[]string{vk(book, bookEN), ck(book)}, "", &access.Preview{Pages: 3}, nil},
		{[]string{"premium", vk(book, bookUncut), ck(book)}, "", nil, nil},
		{[]string{"gold", vk(saga, sagaV), ck(saga)}, "", nil, nil},
		{[]string{gk(channel), vk(members, membersV), ck(members)}, "", nil, nil},
		{[]string{vk(mppv, mppvV), ck(mppv)}, gk(channel), nil, nil},
		{},
		{[]string{vk(fresh, freshV), ck(fresh)}, "", nil, &future},
	} {
		d := ds[i]
		var sell []string
		for _, k := range d.Sell {
			sell = append(sell, k.String())
		}
		req := ""
		if d.Requires != nil {
			req = d.Requires.String()
		}
		if !slices.Equal(sell, want.sell) || req != want.requires || fmt.Sprint(d.Preview) != fmt.Sprint(want.preview) ||
			(len(d.Releases) == 0) != (want.after == nil) || len(d.Releases) > 0 && !d.Releases[0].At.Equal(*want.after) {
			t.Errorf("item %d: sell %v requires %q preview %v after %v", i, sell, req, d.Preview, d.Releases)
		}
	}
	// A member buying members-only PPV needs nothing first.
	ds, err = w.gate.Decide(ctx, member, w.items(mppvV))
	if err != nil || ds[0].Allowed || ds[0].Requires != nil {
		t.Fatalf("member: %+v %v", ds[0], err)
	}
}

// facet writes the listing projection of a version the way the catalog
// does, from its stored rule (Rule.Facet) and memberships.
func (w *world) facet(it access.Item) {
	f := it.Rule.Facet()
	w.exec(`INSERT INTO `+w.table("content_facets")+` (tenant_id, content_id, version_id, kind, languages,
			access_level, access_tiers, access_group_id, access_releases, group_ids, published_at)
		VALUES ($1, $2, $3, 'post', '{en}', $4, $5, $6, $7, $8, now())`,
		tenant, it.Content, it.Version, f.Level, f.Tiers, f.GroupID, f.Releases, it.Groups)
}

// Filter.SQL over content_facets admits, per viewer, exactly the versions
// Decide opens (Hide), and every version (Lock).
func TestFilterSQLMatchesDecide(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	channel, series, tag := w.group("channel", "Studio", ""), w.group("series", "Saga", ""), w.group("tag", "Tag", "")
	rules := []access.Rule{
		{Level: access.Public},
		{Level: access.TierLevel, Tiers: []string{"premium"}},
		{Level: access.TierLevel, Tiers: []string{"gold", "premium"}},
		{Level: access.GroupLevel, Group: channel},
		{Level: access.PPV},
		{Level: access.GroupPPV, Group: channel},
		{Level: access.PPV, Releases: []access.Release{{At: past, Level: access.Public}}},
		{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: future, Level: access.Public}}},
	}
	var versions []string
	for i, r := range rules {
		for j := range 3 {
			c := w.content(r, fmt.Sprintf("c%d-%d", i, j), "")
			versions = append(versions, w.version(c, nil, ""), w.version(c, &access.Rule{Level: access.PPV}, ""))
			switch j {
			case 1:
				w.member(series, c)
			case 2:
				w.member(tag, c)
			}
		}
	}
	page := w.items(versions...)
	for _, it := range page {
		w.facet(it)
	}
	gold, premium, member, buyer, mixed := w.customer(), w.customer(), w.customer(), w.customer(), w.customer()
	w.holds(gold, "gold")
	w.holds(premium, "premium")
	w.holds(member, gk(channel), gk(series))
	w.holds(buyer, ck(page[8].Content), vk(page[9].Content, page[9].Version), vk(page[13].Content, page[13].Version))
	w.holds(mixed, "gold", gk(series), ck(page[30].Content))
	for _, viewer := range []access.Actor{{Anonymous: true}, w.customer(), gold, premium, member, buyer, mixed} {
		mctx := access.WithMemo(ctx)
		f, err := w.gate.Filter(mctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		ds, err := w.gate.Decide(mctx, viewer, page)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for i, d := range ds {
			if allowed, known := f.Allows(page[i]); !known || allowed != d.Allowed {
				t.Errorf("%s: Allows(%d) = %v %v, Decide %v", viewer.ID, i, allowed, known, d.Allowed)
			}
			if d.Allowed {
				want = append(want, page[i].Version)
			}
		}
		for _, mode := range []access.Mode{access.Hide, access.Lock} {
			pred, args, err := f.SQL("f", mode)
			if err != nil {
				t.Fatal(err)
			}
			args["tenant"] = tenant
			rows, err := w.pool.Query(ctx, `SELECT f.version_id FROM `+w.table("content_facets")+` f WHERE f.tenant_id = @tenant AND `+pred+` ORDER BY f.version_id`,
				pgx.NamedArgs(args))
			if err != nil {
				t.Fatalf("%v\n%s", err, pred)
			}
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			expect := slices.Sorted(slices.Values(want))
			if mode == access.Lock {
				expect = slices.Sorted(slices.Values(versions))
			}
			if !slices.Equal(got, expect) {
				t.Errorf("%s %s: SQL admits %d rows, want %d", viewer.ID, mode, len(got), len(expect))
			}
		}
	}
}
