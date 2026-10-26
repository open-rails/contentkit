package critical_test

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckbilling "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// All grants go through the public OpenRails merchant client. A grant is real
// product access, including its persisted time window and revocation history.
func TestCommerceEntitlementBoundaries(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		c.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", Entitled: true, MembershipScopes: []kinds.MembershipScope{kinds.ContentScope}}
		kind := c.Content.Kinds["article"]
		kind.Versions.Hidden, kind.Entities = false, []string{"tag"}
		c.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	poolConfig := w.pool.Config().Copy()
	wire := newTCPCut(t, net.JoinHostPort(poolConfig.ConnConfig.Host, strconv.Itoa(int(poolConfig.ConnConfig.Port))))
	host, port, err := net.SplitHostPort(wire.listener.Addr().String())
	must(t, err)
	portNumber, err := strconv.Atoi(port)
	must(t, err)
	poolConfig.ConnConfig.Host, poolConfig.ConnConfig.Port = host, uint16(portNumber)
	poolConfig.ConnConfig.ConnectTimeout = time.Second
	billingPool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	must(t, err)
	t.Cleanup(billingPool.Close)
	b := attachBilling(t, w, a, func(d *openrails.Deps) { d.Postgres = billingPool })
	startPublicationRuntime(t, a)
	admin := a.engine.Client()
	create := func(title string, rule access.Rule) catalog.Content {
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: &rule,
			Text: map[string]catalog.Text{"en": {Title: title, Body: "Protected manuscript"}}, Versions: []catalog.VersionInput{
				{Languages: []string{"en"}, Access: &rule, Text: map[string]catalog.VersionText{"en": {Title: title + " English"}}},
				{Languages: []string{"de"}, Access: &rule, Text: map[string]catalog.VersionText{"de": {Title: title + " German"}}},
			}})
		must(t, err)
		item, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		for _, version := range item.Versions {
			awaitEditionPublication(t, a, admin, item.ID, version.ID)
		}
		return item
	}
	one, other := create("Granted article", access.Rule{Level: access.PPV}), create("Unrelated article", access.Rule{Level: access.PPV})
	tier := create("Gold tier", access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}})
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "paid-collection", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Paid collection"}}}})
	must(t, err)
	entityItem := create("Entity article", access.Rule{Level: access.EntityLevel, Entity: string(tags[0].ID)})
	keyOf := func(key access.Key, err error) string { must(t, err); return key.String() }
	contentKey := keyOf(access.ContentKey(a.cfg.Scope.AppID.String(), one.ID))
	versionKey := keyOf(access.VersionKey(a.cfg.Scope.AppID.String(), one.ID, one.Versions[0].ID))
	entityKey := keyOf(access.EntityKey(a.cfg.Scope.AppID.String(), string(tags[0].ID)))
	product := func(key string) billing.Product {
		p, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "critical-" + uuid.NewString(), DisplayName: "Critical access", Entitlements: []string{key}})
		must(t, err)
		return *p
	}
	products := map[string]billing.Product{"content": product(contentKey), "version": product(versionKey), "entity": product(entityKey), "tier": product("gold")}
	newReader := func(t *testing.T) (*ck.Client, billing.CustomerID) {
		u, credential := w.person(t, 0)
		id, err := billing.ParseCustomerID(u.ID)
		must(t, err)
		_, err = b.merchant.UpdateCustomer(t.Context(), id, billing.UpdateCustomerParams{})
		must(t, err)
		return a.client(credential), id
	}
	grant := func(t *testing.T, customer billing.CustomerID, p billing.Product, hours *int) billing.ProductAccessGrant {
		rows, err := b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: uuid.NewString(), Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: p.ID, Hours: hours}}})
		must(t, err)
		if len(rows) != 1 {
			t.Fatalf("grant returned %d windows", len(rows))
		}
		return rows[0]
	}
	assertVersion := func(t *testing.T, reader *ck.Client, item catalog.Content, index int, allowed bool) {
		t.Helper()
		got, err := reader.GetVersion(t.Context(), item.Versions[index].ID, ck.GetVersionQuery{})
		must(t, err)
		if got.SelectedVersionID != item.Versions[index].ID || got.Decision.Allowed != allowed || (got.Body != "") != allowed {
			t.Fatalf("grant scope: item=%s version=%d allowed=%v body=%q decision=%+v", item.ID, index, allowed, got.Body, got.Decision)
		}
	}
	t.Run("CK-J06-S01_content_version_entity_and_tier_grants_are_scoped", func(t *testing.T) {
		for _, name := range []string{"content", "version", "entity", "tier"} {
			t.Run(name, func(t *testing.T) {
				reader, id := newReader(t)
				assertVersion(t, reader, one, 0, false)
				grant(t, id, products[name], nil)
				assertVersion(t, reader, one, 0, name == "version")
				assertVersion(t, reader, one, 1, false)
				assertVersion(t, reader, other, 0, false)
				assertVersion(t, reader, tier, 0, name == "tier")
				assertVersion(t, reader, entityItem, 0, name == "entity")
			})
		}
	})
	t.Run("CK-J06-S02_future_window_cannot_open_content_early", func(t *testing.T) {
		reader, id := newReader(t)
		hours := 1
		first := grant(t, id, products["version"], &hours)
		future := grant(t, id, products["version"], &hours)
		if first.EndsAt == nil || !future.StartsAt.Equal(*first.EndsAt) {
			t.Fatal("public grants did not create consecutive access windows")
		}
		must(t, b.merchant.RevokeProductAccess(t.Context(), first.ID, billing.RevokeProductAccessParams{Reason: "Keep only the future window"}))
		before := future.StartsAt.Add(-time.Microsecond)
		b.clock.Advance(before.Sub(b.clock.Now()))
		w.clock.set(before)
		assertVersion(t, reader, one, 0, false)
		b.clock.Advance(time.Microsecond)
		w.clock.set(future.StartsAt)
		assertVersion(t, reader, one, 0, true)
	})
	t.Run("CK-J06-S04_revocation_and_expiry_refuse_fresh_content_grants", func(t *testing.T) {
		for _, mode := range []string{"revoke", "expire"} {
			t.Run(mode, func(t *testing.T) {
				reader, id := newReader(t)
				hours := 1
				window := grant(t, id, products["version"], &hours)
				assertVersion(t, reader, one, 0, true)
				if mode == "revoke" {
					must(t, b.merchant.RevokeProductAccess(t.Context(), window.ID, billing.RevokeProductAccessParams{Reason: "Revoke access"}))
				} else {
					b.clock.Advance(window.EndsAt.Sub(b.clock.Now()))
					w.clock.set(*window.EndsAt)
				}
				assertVersion(t, reader, one, 0, false)
				library, err := reader.GetMeLibrary(t.Context(), ck.GetMeLibraryQuery{})
				must(t, err)
				if len(library.Items) != 0 {
					t.Fatalf("ended access remained in library: %+v", library.Items)
				}
			})
		}
	})
	t.Run("CK-J06-S07_new_price_preserves_content_and_grant_identity", func(t *testing.T) {
		reader, id := newReader(t)
		window := grant(t, id, products["version"], nil)
		p := products["version"]
		_, err := b.merchant.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: p.ID, Key: "purchase", UnitAmount: 10_000_000, Currency: "USD"})
		must(t, err)
		_, err = b.merchant.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: p.ID, Key: "purchase", UnitAmount: 20_000_000, Currency: "USD"})
		must(t, err)
		current, err := b.merchant.GetProduct(t.Context(), p.ID)
		must(t, err)
		if !reflect.DeepEqual(current.Entitlements, []string{versionKey}) {
			t.Fatalf("price replacement changed product rights: %+v", current)
		}
		rows, err := b.merchant.ListProductAccess(t.Context(), billing.ProductAccessListParams{IDs: []billing.ProductAccessID{window.ID}})
		must(t, err)
		if len(rows.Items) != 1 || rows.Items[0].ID != window.ID || rows.Items[0].ProductID != p.ID {
			t.Fatalf("price replacement changed prior grant: %+v", rows)
		}
		assertVersion(t, reader, one, 0, true)
	})
	t.Run("CK-J05-S05_unpublish_and_reschedule_preserve_purchased_rights", func(t *testing.T) {
		reader, customer := newReader(t)
		window := grant(t, customer, products["version"], nil)
		assertVersion(t, reader, one, 0, true)
		_, err := admin.PostContentUnpublish(t.Context(), one.ID)
		must(t, err)
		_, err = reader.GetContent(t.Context(), one.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		// Let withdrawal finish before scheduling a new request so the
		// subsequent clock-only assertion has no outstanding prior release.
		awaitPublicationJobsIdle(t, w, a)
		at := b.clock.Now().Add(time.Hour)
		_, err = admin.PostContentPublish(t.Context(), one.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		_, err = reader.GetContent(t.Context(), one.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		rows, err := b.merchant.ListProductAccess(t.Context(), billing.ProductAccessListParams{IDs: []billing.ProductAccessID{window.ID}})
		must(t, err)
		if len(rows.Items) != 1 || rows.Items[0].RevokedAt != nil || rows.Items[0].ProductID != products["version"].ID {
			t.Fatal("unpublishing revoked or replaced the independent purchase right")
		}
		b.clock.Advance(at.Sub(b.clock.Now()))
		w.clock.set(at)
		_, err = reader.GetVersion(t.Context(), one.Versions[0].ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		wakeDuePublicationJobs(t, a)
		awaitEditionPublication(t, a, admin, one.ID, one.Versions[0].ID)
		assertVersion(t, reader, one, 0, true)
		rows, err = b.merchant.ListProductAccess(t.Context(), billing.ProductAccessListParams{IDs: []billing.ProductAccessID{window.ID}})
		must(t, err)
		if len(rows.Items) != 1 || rows.Items[0].ID != window.ID {
			t.Fatal("runtime re-release replaced the customer's existing grant")
		}
	})
	t.Run("CK-J06-S06_library_respects_app_and_domain_even_when_both_are_purchased", func(t *testing.T) {
		user, credential := w.person(t, 0)
		customer, err := billing.ParseCustomerID(user.ID)
		must(t, err)
		_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		must(t, err)
		foreign := publishedArticle(t, w.apps[1], "Purchased sibling app item")
		foreignKey := keyOf(access.VersionKey(w.apps[1].cfg.Scope.AppID.String(), foreign.ID, foreign.Versions[0].ID))
		otherKey := keyOf(access.VersionKey(a.cfg.Scope.AppID.String(), other.ID, other.Versions[0].ID))
		for _, p := range []billing.Product{products["version"], product(otherKey), product(foreignKey)} {
			grant(t, customer, p, nil)
		}
		_, err = admin.PutContentEntitiesShared(t.Context(), one.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: tags[0].ID}}}, ck.PutContentEntitiesSharedQuery{})
		must(t, err)
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{
			{Name: "main", Hosts: []string{"limited.invalid"}, RequireEntities: map[string][]kinds.EntityRef{"article": {{Kind: "tag", Slug: "paid-collection"}}}},
			{Name: "wide", Hosts: []string{"wide.invalid"}},
		}}
		engine, err := ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		server := mountEngine(t, engine)
		for _, sample := range []struct {
			host string
			want []string
		}{{"limited.invalid", []string{one.ID}}, {"wide.invalid", []string{one.ID, other.ID}}} {
			client := ck.NewClient(server.URL+"/content", &http.Client{Transport: domainTransport{sample.host, credential}})
			page, err := client.GetMeLibrary(t.Context(), ck.GetMeLibraryQuery{})
			must(t, err)
			seen := map[string]bool{}
			for _, item := range page.Items {
				seen[item.ContentID] = true
				want := one.Versions[0].ID
				if item.ContentID == other.ID {
					want = other.Versions[0].ID
				}
				if item.Whole || !slices.Equal(item.VersionIDs, []string{want}) {
					t.Fatalf("edition grant broadened to a gallery or sibling: %+v", item)
				}
			}
			if len(seen) != len(sample.want) || !page.Complete {
				t.Fatalf("%s library=%+v", sample.host, page)
			}
			for _, id := range sample.want {
				if !seen[id] {
					t.Fatalf("%s omitted purchased item %s", sample.host, id)
				}
			}
		}
		sibling := w.apps[1]
		siblingConfig, deps := sibling.cfg, sibling.deps
		siblingConfig.Database.SkipMigrations = true
		deps.Billing = ckbilling.Billing(b.merchant)
		siblingEngine, err := ck.New(t.Context(), siblingConfig, deps)
		must(t, err)
		defer func() { must(t, siblingEngine.Close(context.Background())) }()
		siblingServer := mountEngine(t, siblingEngine)
		client := ck.NewClient(siblingServer.URL+"/content", &http.Client{Transport: bearerTransport{credential, http.DefaultTransport}})
		page, err := client.GetMeLibrary(t.Context(), ck.GetMeLibraryQuery{})
		must(t, err)
		if len(page.Items) != 1 || page.Items[0].ContentID != foreign.ID {
			t.Fatalf("sibling library crossed app boundary: %+v", page)
		}
	})
	t.Run("CK-J06-S05_unavailable_entitlement_authority_refuses_protected_access", func(t *testing.T) {
		reader, id := newReader(t)
		grant(t, id, products["version"], nil)
		assertVersion(t, reader, one, 0, true)
		wire.cut()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if _, err := b.merchant.CheckEntitlements(ctx, billing.CheckEntitlementsParams{CustomerID: id, Entitlements: []string{versionKey}}); err == nil {
			t.Fatal("fault did not disconnect the real entitlement store")
		}
		locked, err := reader.GetContent(ctx, one.ID, ck.GetContentQuery{})
		must(t, err)
		if locked.Decision.Allowed || !locked.Decision.Unknown || locked.Body != "" || len(locked.Decision.Sell) != 0 {
			t.Fatalf("entitlement outage exposed body or offered a second purchase: %+v", locked.Decision)
		}
		_, err = reader.GetMeLibrary(ctx, ck.GetMeLibraryQuery{})
		requireError(t, err, 503, "unavailable")
	})
}

// Await completion through the editor's actual version read. Slow polling
// stays below HTTP issuance limits while the real River worker runs.
func awaitEditionPublication(t *testing.T, app *application, client *ck.Client, content, version string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last catalog.Version
	var err error
	for time.Now().Before(deadline) {
		wakeDuePublicationJobs(t, app)
		last, err = client.GetContentVersion(t.Context(), content, version, ck.GetContentVersionQuery{})
		if err == nil && last.Live {
			return
		}
		time.Sleep(550 * time.Millisecond)
	}
	t.Fatalf("edition publication did not complete: version=%s current=%+v error=%v", version, last, err)
}

func awaitPublicationJobsIdle(t *testing.T, w *world, app *application) {
	t.Helper()
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, app)
		var pending int
		err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+`
 WHERE kind IN ('contentkit_version_release','contentkit_version_publish') AND state IN ('available','running','retryable')`).Scan(&pending)
		must(t, err)
		return pending == 0
	})
}
