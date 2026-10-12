package openrails_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/offers"
)

// catalogStaff holds every catalog permission for the actor "staff".
type catalogStaff struct{}

func (catalogStaff) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff", nil
}

// The catalog over a real embedded OpenRails: held group, content and
// version keys open what the rules say in listings and reads, a deleted item
// stays readable to holders of its own keys, and a title change renames the
// item's product.
func TestCatalogOverOpenRails(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	var cat *catalog.Store
	grp, err := groups.New(groups.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: w.kinds,
		ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error { return cat.ContentChanged(ctx, q, ids) }})
	if err != nil {
		t.Fatal(err)
	}
	if cat, err = catalog.New(ctx, catalog.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: w.kinds,
		Auth: testauth.Auth{Authz: catalogStaff{}}, Gate: w.gate, Groups: grp, Offers: w.offers}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	staff := testauth.Actor(access.Actor{ID: "staff"}, catalogStaff{})
	made, err := grp.CreateGroups(ctx, []groups.GroupInput{{Kind: "channel", Slug: "studio", OwnerUserID: "owner",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Studio"}}}})
	if err != nil {
		t.Fatal(err)
	}
	studio := string(made[0].ID)
	post := func(title string, listing string, rule access.Rule, languages ...string) string {
		t.Helper()
		in := catalog.ContentInput{Kind: "post", Listing: listing, Access: &rule, Text: map[string]catalog.Text{"en": {Title: title}}}
		for _, l := range languages {
			in.Versions = append(in.Versions, catalog.VersionInput{Languages: []string{l}})
		}
		ids, err := cat.Create(ctx, staff, []catalog.ContentInput{in})
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.Publish(ctx, staff, []catalog.Publish{{ContentID: ids[0]}}); err != nil {
			t.Fatal(err)
		}
		return ids[0]
	}
	free := post("Free", catalog.ListingListed, access.Rule{Level: access.Public}, "en")
	members := post("Members", catalog.ListingMembers, access.Rule{Level: access.GroupLevel, Group: studio}, "en")
	ppv := post("Night Train", catalog.ListingListed, access.Rule{Level: access.PPV}, "en", "ja")
	read := func(a access.Actor, ids ...string) []*catalog.Content {
		t.Helper()
		ctx := access.WithMemo(ctx)
		v, err := cat.Viewer(ctx, catalog.ViewerOptions{Actor: a})
		if err != nil {
			t.Fatal(err)
		}
		got, err := cat.Contents(ctx, v, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	versions := read(staff, ppv)[0].Versions
	en, ja := versions[0].ID, versions[1].ID

	max, bob := w.customer(), w.customer()
	w.holds(max, gk(studio), vk(ppv, ja))
	w.holds(bob, ck(ppv))
	listed := func(a access.Actor, mode access.Mode) []string {
		t.Helper()
		ctx := access.WithMemo(ctx)
		v, err := cat.Viewer(ctx, catalog.ViewerOptions{Actor: a, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		pred, args := v.Listed("f")
		args["tenant"] = tenant
		rows, err := w.pool.Query(ctx, `SELECT DISTINCT f.content_id FROM `+w.table("content_facets")+` f WHERE f.tenant_id = @tenant AND `+pred+` ORDER BY 1`, args)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	sorted := func(ids ...string) []string { return slices.Sorted(slices.Values(ids)) }
	for _, c := range []struct {
		name   string
		viewer access.Actor
		mode   access.Mode
		want   []string
	}{
		{"anonymous lists what is listed", access.Actor{Anonymous: true}, access.Lock, sorted(free, ppv)},
		{"anonymous hides what it does not open", access.Actor{Anonymous: true}, access.Hide, sorted(free)},
		{"a member lists the members listing", max, access.Lock, sorted(free, members, ppv)},
		{"a member and version buyer opens all three", max, access.Hide, sorted(free, members, ppv)},
		{"a content buyer opens the item", bob, access.Hide, sorted(free, ppv)},
	} {
		if got := listed(c.viewer, c.mode); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	got := read(max, members, ppv)
	if !got[0].Versions[0].Decision.Allowed {
		t.Fatalf("the member's members-only item %+v", got[0].Versions[0].Decision)
	}
	if d := got[1].Versions; d[0].Decision.Allowed || !slices.Equal(d[0].Decision.Sell, []string{vk(ppv, en), ck(ppv)}) || !d[1].Decision.Allowed {
		t.Fatalf("the version buyer's decisions %+v %+v", d[0].Decision, d[1].Decision)
	}

	// Deleted: holders of its content key read every version that was live,
	// holders of a version key that version; nobody else.
	if err := cat.Delete(ctx, staff, []string{ppv}); err != nil {
		t.Fatal(err)
	}
	if got := read(access.Actor{Anonymous: true}, ppv)[0]; got != nil {
		t.Fatalf("anonymous reads the deleted item %+v", got)
	}
	if got := read(bob, ppv)[0]; got == nil || got.Live || len(got.Versions) != 2 {
		t.Fatalf("the content buyer's deleted item %+v", got)
	}
	if got := read(max, ppv)[0]; got == nil || len(got.Versions) != 1 || got.Versions[0].ID != ja || !got.Versions[0].Decision.Allowed {
		t.Fatalf("the version buyer's deleted item %+v", got)
	}
	targets := []catalog.Target{{Content: ppv}, {Content: ppv, Version: en}, {Content: ppv, Version: ja}}
	sights, err := cat.See(access.WithMemo(ctx), max, targets)
	if err != nil || len(sights) != 2 || sights[targets[0]] != (catalog.Sight{Visible: true}) || sights[targets[2]] != (catalog.Sight{Visible: true}) {
		t.Fatalf("the version buyer sees %+v %v", sights, err)
	}
	opens, err := cat.Open(access.WithMemo(ctx), max, targets[1:])
	if err != nil || len(opens) != 1 || !opens[targets[2]].Opens {
		t.Fatalf("the version buyer opens %+v %v", opens, err)
	}
	if err := cat.Restore(ctx, staff, []string{ppv}); err != nil {
		t.Fatal(err)
	}

	// The product's name follows the title.
	target := offers.Target{Kind: offers.ContentTarget, ID: ppv}
	if _, err := w.offers.Put(ctx, []offers.Put{{Target: target, Terms: offers.Terms{Amount: 4_990_000}}}); err != nil {
		t.Fatal(err)
	}
	sync := func() string {
		t.Helper()
		if err := w.offers.Sync(ctx, []offers.Target{target}); err != nil {
			t.Fatal(err)
		}
		p, _ := w.product(ck(ppv))
		return p.DisplayName
	}
	if name := sync(); name != "Night Train" {
		t.Fatalf("product %q", name)
	}
	if err := cat.PutText(ctx, staff, []catalog.TextPut{{ContentID: ppv, Language: "en", Text: &catalog.Text{Title: "Night Train II"}}}); err != nil {
		t.Fatal(err)
	}
	if o := w.offer(target); o.Synced {
		t.Fatalf("a retitled offer is synced %+v", o)
	}
	if name := sync(); name != "Night Train II" {
		t.Fatalf("renamed product %q", name)
	}
}
