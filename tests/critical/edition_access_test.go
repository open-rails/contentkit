package critical_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckbilling "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/openrails/billing"
)

// Grants use the real billing service; the gate, SQL filter, HTTP reader and
// purchased library must agree on the same independently owned editions.
func TestEditionEntitlementIsolation(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		kind := c.Content.Kinds["article"]
		kind.Versions.Hidden = false
		c.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	b := attachBilling(t, w, a)
	create := func(title string) catalog.Content {
		item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{
			Kind: "article", Access: &access.Rule{Level: access.PPV},
			Text:     map[string]catalog.Text{"en": {Title: title, Body: "Protected edition body"}},
			Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"de"}}},
		})
		must(t, err)
		item, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		return item
	}
	one, other := create("Edition grant"), create("Other edition")
	key := func(k access.Key, err error) string { must(t, err); return k.String() }
	parent := key(access.ContentKey(a.cfg.Scope.AppID.String(), one.ID))
	first := key(access.VersionKey(a.cfg.Scope.AppID.String(), one.ID, one.Versions[0].ID))
	second := key(access.VersionKey(a.cfg.Scope.AppID.String(), one.ID, one.Versions[1].ID))
	wrongParent := key(access.VersionKey(a.cfg.Scope.AppID.String(), other.ID, one.Versions[1].ID))
	foreign := key(access.VersionKey(w.apps[1].cfg.Scope.AppID.String(), one.ID, one.Versions[1].ID))
	items := []access.Item{
		{Content: one.ID, Version: one.Versions[0].ID, Rule: access.Rule{Level: access.PPV}},
		{Content: one.ID, Version: one.Versions[1].ID, Rule: access.Rule{Level: access.PPV}},
		{Content: other.ID, Version: other.Versions[0].ID, Rule: access.Rule{Level: access.PPV}},
		{Content: other.ID, Version: other.Versions[1].ID, Rule: access.Rule{Level: access.PPV}},
	}
	for _, test := range []struct {
		name   string
		grants []string
		open   []string
	}{
		{"parent_is_not_an_audience_grant", []string{parent}, nil},
		{"one_edition_never_opens_its_sibling", []string{first}, []string{one.Versions[0].ID}},
		{"wrong_parent_and_foreign_app_are_not_edition_grants", []string{first, wrongParent, foreign}, []string{one.Versions[0].ID}},
		{"two_explicit_grants_remain_independent", []string{first, second}, []string{one.Versions[0].ID, one.Versions[1].ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			user, credential := w.person(t, 0)
			customer, err := billing.ParseCustomerID(user.ID)
			must(t, err)
			_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
			must(t, err)
			product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{
				Key: "edition-" + uuid.NewString(), DisplayName: "Edition access", Entitlements: test.grants,
			})
			must(t, err)
			_, err = b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{
				IdempotencyKey: uuid.NewString(), Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}},
			})
			must(t, err)
			g, err := access.NewGate(access.GateConfig{Tenant: a.cfg.Scope.AppID.String(), Kinds: a.cfg.Content, Billing: ckbilling.Billing(b.merchant), Now: w.clock.now})
			must(t, err)
			actor := access.Actor{ID: user.ID}
			ctx := access.WithMemo(t.Context())
			f, err := g.Filter(ctx, actor)
			must(t, err)
			for _, useFilter := range []bool{false, true} {
				decideCtx := t.Context()
				if useFilter {
					decideCtx = ctx
				}
				decisions, err := g.Decide(decideCtx, actor, items)
				must(t, err)
				for i, item := range items {
					want := slices.Contains(test.open, item.Version)
					if decisions[i].Allowed != want || decisions[i].Unknown {
						t.Fatalf("memo=%v version=%s decision=%+v want=%v", useFilter, item.Version, decisions[i], want)
					}
					for _, offered := range decisions[i].Sell {
						if offered.Kind() == access.KeyContent || offered.Kind() == access.KeyVersion && (offered.Content() != item.Content || offered.Version() != item.Version) {
							t.Fatalf("version %s offered another item's right: %s", item.Version, offered.String())
						}
					}
					allowed, known := f.Allows(item)
					if !known || allowed != want {
						t.Fatalf("filter version=%s allowed=%v known=%v want=%v", item.Version, allowed, known, want)
					}
				}
				foreignKey, err := access.ParseKey(foreign)
				must(t, err)
				legacyKey, err := access.ParseKey(parent)
				must(t, err)
				held, err := g.Holds(decideCtx, actor, []access.Key{foreignKey, legacyKey})
				must(t, err)
				if held[0] || held[1] != slices.Contains(test.grants, parent) {
					t.Fatalf("memo=%v scoped/audit key reads=%v", useFilter, held)
				}
			}
			predicate, args, err := f.SQL("f", access.Hide)
			must(t, err)
			args["fixture_tenant"], args["fixture_parents"] = a.cfg.Scope.AppID.String(), []string{one.ID, other.ID}
			rows, err := w.pool.Query(t.Context(), "SELECT f.version_id FROM "+pgx.Identifier{w.schema, "content_facets"}.Sanitize()+" f WHERE f.tenant_id=@fixture_tenant AND f.content_id=ANY(@fixture_parents::text[]) AND "+predicate+" ORDER BY f.version_id", pgx.NamedArgs(args))
			must(t, err)
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			must(t, err)
			want := slices.Clone(test.open)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("SQL grants %v, want %v", got, want)
			}
			reader := a.client(credential)
			for _, item := range items {
				got, err := reader.GetVersion(t.Context(), item.Version, ck.GetVersionQuery{})
				must(t, err)
				if got.Decision.Allowed != slices.Contains(test.open, item.Version) {
					t.Fatalf("HTTP version %s decision=%+v", item.Version, got.Decision)
				}
			}
			library, err := reader.GetMeLibrary(t.Context(), ck.GetMeLibraryQuery{})
			must(t, err)
			owned := []string{}
			for _, item := range library.Items {
				if item.Whole {
					t.Fatal("library advertised a whole-gallery grant")
				}
				owned = append(owned, item.VersionIDs...)
			}
			slices.Sort(owned)
			if !library.Complete || !slices.Equal(owned, want) {
				t.Fatalf("library=%+v, want editions %v", library, want)
			}
			// A missing edition ID cannot turn the gallery into an access unit.
			decisions, err := g.Decide(ctx, actor, []access.Item{{Content: one.ID, Rule: access.Rule{Level: access.PPV}}})
			must(t, err)
			if decisions[0].Allowed || len(decisions[0].Sell) != 0 {
				t.Fatalf("gallery-only request gained access: %+v", decisions[0])
			}
		})
	}
}

func TestChapterSubsetPreviewIsRefused(t *testing.T) {
	for _, p := range []kinds.Preview{{Segments: 1}, {Pages: 2, Segments: 1}} {
		if err := p.Validate(); err == nil {
			t.Fatalf("chapter subset accepted: %+v", p)
		}
		rule := access.Rule{Level: access.PPV, Preview: &p}
		if err := rule.Validate(kinds.Config{}); err == nil {
			t.Fatalf("rule accepted chapter subset: %+v", rule)
		}
	}
	p := kinds.Preview{Pages: 2}
	must(t, p.Validate())
}
