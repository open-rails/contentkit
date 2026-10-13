package openrails_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/offers"
)

const tenant = "onlydemo"

// counting counts the CheckEntitlements requests the adapter sends.
type counting struct {
	*openrails.Client
	n atomic.Int64
}

func (c *counting) CheckEntitlements(ctx context.Context, p billing.CheckEntitlementsParams, o ...openrails.RequestOption) (*billing.EntitlementCheck, error) {
	c.n.Add(1)
	return c.Client.CheckEntitlements(ctx, p, o...)
}

func (c *counting) take() int { return int(c.n.Swap(0)) }

// world is ContentKit's schema and a real embedded OpenRails on the
// CONTENTKIT_TEST_URL database (CONTENTKIT_TEST_REQUIRE_DB=1 fails instead of
// skipping), with a Gate and the offers Service over them.
type world struct {
	t       *testing.T
	pool    *pgxpool.Pool
	schema  string
	bill    *openrails.Client
	client  *counting
	billing access.Billing
	kinds   kinds.Config
	gate    *access.Gate
	offers  *offers.Service
	n       int
}

// newWorld builds the world; edits adjust OpenRails' configuration.
func newWorld(t *testing.T, edits ...func(*openrails.Config)) *world {
	t.Helper()
	if os.Getenv("CONTENTKIT_TEST_URL") == "" && os.Getenv("CONTENTKIT_TEST_REQUIRE_DB") == "1" {
		t.Fatal("CONTENTKIT_TEST_URL is required")
	}
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	w := &world{t: t, pool: pool, schema: pgtest.Schema(t, ctx, pool)}
	orSchema := "ckor_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{orSchema}.Sanitize()+" CASCADE")
	})
	cfg := openrails.Config{Database: openrails.DatabaseConfig{Schema: orSchema, RiverSchema: orSchema}, TestMode: openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesReadOnly, Merchants: map[string]openrails.MerchantDeclaration{tenant: {DisplayName: "OnlyDemo"}}}
	for _, edit := range edits {
		edit(&cfg)
	}
	engine, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	if w.bill, err = engine.MerchantBySlug(tenant); err != nil {
		t.Fatal(err)
	}
	w.client = &counting{Client: w.bill}
	w.billing = ckopenrails.Billing(w.client)
	w.kinds = kinds.Config{Languages: []string{"en", "ja"}, Tiers: []string{"premium", "gold"}, TokenClaims: []string{"premium"},
		Kinds:      map[string]kinds.Kind{"post": {Route: "p", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}},
		GroupKinds: map[string]kinds.GroupKind{"channel": {Owner: "user", Sellable: true}, "series": {Sellable: true, Ordered: true}, "tag": {}}}
	if err := w.kinds.Validate(); err != nil {
		t.Fatal(err)
	}
	if w.gate, err = access.NewGate(access.GateConfig{Tenant: tenant, Kinds: w.kinds, Billing: w.billing}); err != nil {
		t.Fatal(err)
	}
	w.offers, err = offers.New(offers.Options{Pool: pool, Schema: w.schema, Tenant: tenant, Kinds: w.kinds, Billing: w.billing, Gate: w.gate,
		Config: offers.Config{Minimum: map[offers.TargetKind]offers.Amount{offers.ContentTarget: 500_000, offers.GroupTarget: 1_000_000}}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) id() string {
	w.n++
	return contentref.IDAt(time.Unix(1_700_000_000+int64(w.n), 0))
}

func (w *world) table(name string) string { return pgx.Identifier{w.schema, name}.Sanitize() }

func (w *world) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.pool.Exec(w.t.Context(), sql, args...); err != nil {
		w.t.Fatalf("%v\n%s", err, sql)
	}
}

func ruleJSON(r *access.Rule) []byte {
	if r == nil {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return b
}

// content writes a published content item with its rule, English title and
// creator.
func (w *world) content(rule access.Rule, title, creator string) string {
	id := w.id()
	w.exec(`INSERT INTO `+w.table("content_items")+` (tenant_id, id, kind, state, published_at, access, created_by) VALUES ($1, $2, 'post', 'published', now(), $3, $4)`,
		tenant, id, ruleJSON(&rule), creator)
	w.exec(`INSERT INTO `+w.table("content_text")+` (tenant_id, content_id, language, title) VALUES ($1, $2, 'en', $3)`, tenant, id, title)
	if creator != "" {
		w.exec(`INSERT INTO `+w.table("content_creators")+` (tenant_id, content_id, user_id) VALUES ($1, $2, $3)`, tenant, id, creator)
	}
	return id
}

// version writes a published version of content with an optional override.
func (w *world) version(content string, override *access.Rule, label string) string {
	id := w.id()
	w.exec(`INSERT INTO `+w.table("content_versions")+` (tenant_id, id, content_id, languages, state, published_at, access, label)
		VALUES ($1, $2, $3, '{en}', 'published', now(), $4, $5)`, tenant, id, content, ruleJSON(override), label)
	return id
}

// group writes an active group with its English name.
func (w *world) group(kind, name, owner string) string {
	id := w.id()
	var o *string
	if owner != "" {
		o = &owner
	}
	w.exec(`INSERT INTO `+w.table("content_groups")+` (tenant_id, id, kind, slug, owner_user_id) VALUES ($1, $2, $3, $4, $5)`,
		tenant, id, kind, strings.ToLower(strings.ReplaceAll(name, " ", "-")), o)
	w.exec(`INSERT INTO `+w.table("content_group_names")+` (tenant_id, group_id, language, kind, name) VALUES ($1, $2, 'en', 'name', $3)`, tenant, id, name)
	return id
}

func (w *world) member(group, content string) {
	w.exec(`INSERT INTO `+w.table("content_memberships")+` (tenant_id, group_id, content_id) VALUES ($1, $2, $3)`, tenant, group, content)
}

// items reads versions' Items the way listings do: the effective rule from
// content and version, the groups from memberships.
func (w *world) items(versions ...string) []access.Item {
	w.t.Helper()
	out := make([]access.Item, len(versions))
	for i, v := range versions {
		var content string
		var crule, vrule []byte
		var groups []string
		if err := w.pool.QueryRow(w.t.Context(), `SELECT c.id, c.access, v.access,
				coalesce((SELECT array_agg(m.group_id ORDER BY m.group_id) FROM `+w.table("content_memberships")+` m
					WHERE m.tenant_id = c.tenant_id AND m.content_id = c.id AND (m.version_id IS NULL OR m.version_id = v.id)), '{}')
			FROM `+w.table("content_versions")+` v JOIN `+w.table("content_items")+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
			WHERE v.tenant_id = $1 AND v.id = $2`, tenant, v).Scan(&content, &crule, &vrule, &groups); err != nil {
			w.t.Fatal(err)
		}
		var rule access.Rule
		if err := json.Unmarshal(crule, &rule); err != nil {
			w.t.Fatal(err)
		}
		if vrule != nil {
			var o access.Rule
			if err := json.Unmarshal(vrule, &o); err != nil {
				w.t.Fatal(err)
			}
			rule = access.Effective(rule, &o)
		}
		if err := rule.Validate(w.kinds); err != nil {
			w.t.Fatal(err)
		}
		out[i] = access.Item{Content: content, Version: v, Rule: rule, Groups: groups}
	}
	return out
}

// customer is a new OpenRails customer and the viewer it signs in as.
func (w *world) customer() access.Actor {
	w.t.Helper()
	id := billing.CustomerID(uuid.New())
	if _, err := w.bill.UpdateCustomer(w.t.Context(), id, billing.UpdateCustomerParams{}); err != nil {
		w.t.Fatal(err)
	}
	return access.Actor{ID: id.String(), Kind: auth.SubjectUser}
}

// holds grants viewer keys free, as a purchase would.
func (w *world) holds(viewer access.Actor, keys ...string) {
	w.t.Helper()
	ctx := w.t.Context()
	product, err := w.bill.CreateProduct(ctx, billing.CreateProductParams{Key: "fixture-" + uuid.NewString()[:8], DisplayName: "Fixture", Entitlements: keys})
	if err != nil {
		w.t.Fatal(err)
	}
	customer, _ := billing.ParseCustomerID(viewer.ID)
	if _, err := w.bill.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{
		Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) holdsNow(viewer access.Actor, key string) bool {
	w.t.Helper()
	customer, _ := billing.ParseCustomerID(viewer.ID)
	res, err := w.bill.CheckEntitlements(w.t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{key}})
	if err != nil {
		w.t.Fatal(err)
	}
	return res.Entitlements[key]
}

func ck(id string) string     { return must(access.ContentKey(tenant, id)).String() }
func vk(c, v string) string   { return must(access.VersionKey(tenant, c, v)).String() }
func gk(id string) string     { return must(access.GroupKey(tenant, id)).String() }
func key(s string) access.Key { return must(access.ParseKey(s)) }
func uid(n int) string        { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }
func ptr[T any](v T) *T       { return &v }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// claims is a verified token carrying entitlement claims (access.Entitled).
type claims struct {
	id   string
	ents []string
}

func (c claims) Identity() auth.Identity {
	return auth.Identity{Subject: c.id, SubjectKind: auth.SubjectUser}
}
func (c claims) Entitlements() []string { return c.ents }
