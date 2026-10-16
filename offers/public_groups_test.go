package offers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
)

type heldForPolicyTest struct {
	access.Billing
	keys []string
}

func (b *heldForPolicyTest) Held(_ context.Context, _ string, q access.Query) (access.Answer, error) {
	out := access.Answer{Keys: map[string]bool{}, Held: map[string]access.HeldKeys{}}
	for _, prefix := range q.Prefixes {
		h := access.HeldKeys{}
		for _, key := range b.keys {
			if strings.HasPrefix(key, prefix) {
				h.Keys = append(h.Keys, key)
			}
		}
		out.Held[prefix] = h
	}
	for _, key := range q.Keys {
		for _, held := range b.keys {
			if held == key {
				out.Keys[key] = true
			}
		}
	}
	return out, nil
}

func TestInternalGroupPublicOffersAndLibrary(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	cfg := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader"}}, GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tags", Sellable: true}}}
	billing := &heldForPolicyTest{}
	gate, err := access.NewGate(access.GateConfig{Tenant: "policy", Kinds: cfg, Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg, Billing: billing, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	public, internal := contentref.NewID(), contentref.NewID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []struct {
		id, slug string
		internal bool
	}{{public, "visible", false}, {internal, "restricted", true}} {
		exec(`INSERT INTO `+table("content_groups")+` (tenant_id,id,kind,slug,internal) VALUES ('policy',$1,'tag',$2,$3)`, g.id, g.slug, g.internal)
		exec(`INSERT INTO `+table("content_offers")+` (tenant_id,id,target_kind,target_id,terms) VALUES ('policy',$1,'group',$2,'{"amount":0,"currency":"USD"}')`, contentref.NewID(), g.id)
		key, _ := access.GroupKey("policy", g.id)
		billing.keys = append(billing.keys, key.String())
	}
	publicKey, _ := access.GroupKey("policy", public)
	internalKey, _ := access.GroupKey("policy", internal)
	sales, err := s.Sales(ctx, []string{internalKey.String(), publicKey.String()})
	if err != nil || len(sales) != 1 || sales[publicKey.String()].Key != publicKey {
		t.Fatalf("public sales: %+v %v", sales, err)
	}
	actor := access.Actor{ID: "reader"}
	if _, err := s.GroupAccess(ctx, actor, []string{internal}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("internal access metadata: %v", err)
	}
	if _, err := s.Join(ctx, actor, []string{internal}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("internal join: %v", err)
	}
	joined, err := s.Joined(ctx, actor)
	if err != nil || len(joined) != 1 || joined[0] != public {
		t.Fatalf("joined: %v %v", joined, err)
	}
	// Purchased content still obeys the domain boundary. The underlying held
	// keys remain unchanged and serve again on the unrestricted domain.
	visibleContent, blockedContent := contentref.NewID(), contentref.NewID()
	for _, c := range []struct {
		id     string
		groups []string
	}{{visibleContent, []string{}}, {blockedContent, []string{internal}}} {
		vid := contentref.NewID()
		exec(`INSERT INTO `+table("content_items")+` (tenant_id,id,kind,state) VALUES ('policy',$1,'gallery','published')`, c.id)
		exec(`INSERT INTO `+table("content_versions")+` (tenant_id,id,content_id,languages,state) VALUES ('policy',$1,$2,ARRAY['en'],'published')`, vid, c.id)
		exec(`INSERT INTO `+table("content_facets")+` (tenant_id,content_id,version_id,kind,listing,languages,access_level,group_ids) VALUES ('policy',$1,$2,'gallery','listed',ARRAY['en'],'public',$3)`, c.id, vid, c.groups)
		key, _ := access.ContentKey("policy", c.id)
		billing.keys = append(billing.keys, key.String())
	}
	s.kinds.Domains = []kinds.Domain{{Name: "safe", Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false}}}}}}
	page, err := s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ContentID != visibleContent {
		t.Fatalf("domain library: %+v %v", page, err)
	}
	s.kinds.Domains = nil
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("unrestricted library: %+v %v", page, err)
	}
	s.kinds.Domains = []kinds.Domain{{Name: "missing", Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "missing"}}}}}}
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("missing requirement: %+v %v", page, err)
	}
	locked := false
	s.kinds.Domains = []kinds.Domain{{Name: "missing-hidden", Kinds: map[string]kinds.KindPolicy{"gallery": {Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "missing"}, Visible: false, UserToggle: &locked}}}}}}
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("missing hard exclusion: %+v %v", page, err)
	}

	// A missing gallery requirement must not hide an unrelated purchased blog.
	blog, version := contentref.NewID(), contentref.NewID()
	exec(`INSERT INTO `+table("content_items")+` (tenant_id,id,kind,state) VALUES ('policy',$1,'blog','published')`, blog)
	exec(`INSERT INTO `+table("content_versions")+` (tenant_id,id,content_id,languages,state) VALUES ('policy',$1,$2,ARRAY['en'],'published')`, version, blog)
	exec(`INSERT INTO `+table("content_facets")+` (tenant_id,content_id,version_id,kind,listing,languages,access_level) VALUES ('policy',$1,$2,'blog','listed',ARRAY['en'],'public')`, blog, version)
	key, _ := access.ContentKey("policy", blog)
	billing.keys = append(billing.keys, key.String())
	s.kinds.Domains = []kinds.Domain{{Name: "scoped", Kinds: map[string]kinds.KindPolicy{"gallery": {RequireGroups: []kinds.GroupRef{{Kind: "tag", Slug: "missing"}}}}}}
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ContentID != blog {
		t.Fatalf("kind-scoped library: %+v %v", page, err)
	}

}
