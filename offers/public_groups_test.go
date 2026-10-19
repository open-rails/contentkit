package offers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	riverhelpers "github.com/open-rails/helpers/river"
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
	s.kinds.ContentFilters.Domains, s.kinds.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false}}}}, []string{"gallery"}
	page, err := s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ContentID != visibleContent {
		t.Fatalf("domain library: %+v %v", page, err)
	}
	s.kinds.ContentFilters.Domains = nil
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("unrestricted library: %+v %v", page, err)
	}
	s.kinds.ContentFilters.Domains, s.kinds.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", RequireEntities: map[string][]kinds.GroupRef{"gallery": []kinds.GroupRef{{Kind: "tag", Slug: "missing"}}}}}, []string{"gallery"}
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("missing requirement: %+v %v", page, err)
	}
	locked := false
	s.kinds.ContentFilters.Domains, s.kinds.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "missing"}, Visible: false, UserToggle: &locked}}}}, []string{"gallery"}
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
	s.kinds.ContentFilters.Domains, s.kinds.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", RequireEntities: map[string][]kinds.GroupRef{"gallery": []kinds.GroupRef{{Kind: "tag", Slug: "missing"}}}}}, []string{"gallery"}
	page, err = s.Library(ctx, actor, LibraryQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ContentID != blog {
		t.Fatalf("kind-scoped library: %+v %v", page, err)
	}

}

// Reconcile passes over the same unsynced offers keep one waiting sync per
// target; once that sync is done, a later revision queues another.
func TestReconcileQueuesOneSyncPerTarget(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	riverSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	cfg := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader"}}}
	gate, err := access.NewGate(access.GateConfig{Tenant: "policy", Kinds: cfg, Billing: &heldForPolicyTest{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg, Billing: &heldForPolicyTest{}, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if s.client, err = river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema}); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		exec(`INSERT INTO `+pgx.Identifier{schema, "content_offers"}.Sanitize()+` (tenant_id,id,target_kind,target_id,terms) VALUES ('policy',$1,'content',$2,'{"amount":0,"currency":"USD"}')`,
			contentref.NewID(), contentref.NewID())
	}
	jobs := pgx.Identifier{riverSchema, "river_job"}.Sanitize()
	waiting := func(want int) {
		t.Helper()
		var n, targets int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT args->'target') FROM `+jobs+` WHERE kind = 'contentkit_offer_sync' AND state <> 'completed'`).Scan(&n, &targets); err != nil {
			t.Fatal(err)
		}
		if n != want || targets != want {
			t.Fatalf("%d waiting syncs for %d targets, want %d for %d", n, targets, want, want)
		}
	}
	for range 3 {
		if err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waiting(2)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Kind: ContentTarget, ID: contentref.NewID()}
	if err := s.enqueue(ctx, tx, []Target{target, target}); err != nil {
		t.Fatalf("a batch naming one target twice: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE ` + jobs + ` SET state = 'completed', finalized_at = now()`)
	exec(`UPDATE ` + pgx.Identifier{schema, "content_offers"}.Sanitize() + ` SET revision = revision + 1`)
	waiting(0)
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waiting(2)
}

// joinBilling keeps products and free grants in memory; held keys follow the
// grants. before, when set, runs at the start of Products or Grant.
type joinBilling struct {
	heldForPolicyTest
	products        map[string]access.Product
	grants, revokes int
	beforeProducts  func()
	beforeGrant     func() error
	beforeRevoke    func(context.Context) error
	replayErr       error
	attempts        map[string][]string
	windows         map[string]string
}

func (b *joinBilling) Products(_ context.Context, keys []string) (map[string]access.Product, error) {
	if f := b.beforeProducts; f != nil {
		b.beforeProducts = nil
		f()
	}
	out := map[string]access.Product{}
	for _, k := range keys {
		if p, ok := b.products[k]; ok {
			out[k] = p
		}
	}
	return out, nil
}

func (b *joinBilling) StopRenewals(context.Context, []string) error { return nil }

func (b *joinBilling) keyOf(product string) string {
	for k, p := range b.products {
		if p.ID == product {
			return k
		}
	}
	return ""
}

func (b *joinBilling) Grant(_ context.Context, key string, grants []access.Grant) ([]string, error) {
	b.grants++
	if ids, ok := b.attempts[key]; ok {
		if b.replayErr != nil {
			return nil, b.replayErr
		}
		return ids, nil
	}
	var err error
	if f := b.beforeGrant; f != nil {
		b.beforeGrant = nil
		err = f()
	}
	if b.attempts == nil {
		b.attempts = make(map[string][]string)
		b.windows = make(map[string]string)
	}
	var ids []string
	for _, g := range grants {
		id := contentref.NewID()
		ids = append(ids, id)
		b.windows[id] = b.keyOf(g.ProductID)
		if k := b.keyOf(g.ProductID); k != "" && !slices.Contains(b.keys, k) {
			b.keys = append(b.keys, k)
		}
	}
	b.attempts[key] = ids
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (b *joinBilling) RevokeGrants(ctx context.Context, ids []string) error {
	b.revokes++
	if b.beforeRevoke != nil {
		if err := b.beforeRevoke(ctx); err != nil {
			return err
		}
	}
	for _, id := range ids {
		key := b.windows[id]
		delete(b.windows, id)
		stillHeld := false
		for _, k := range b.windows {
			stillHeld = stillHeld || k == key
		}
		if !stillHeld {
			b.keys = slices.DeleteFunc(b.keys, func(k string) bool { return k == key })
		}
	}
	return nil
}

func (b *joinBilling) Revoke(_ context.Context, grants []access.Grant) error {
	b.revokes++
	for _, g := range grants {
		b.keys = slices.DeleteFunc(b.keys, func(k string) bool { return k == b.keyOf(g.ProductID) })
	}
	return nil
}

// A free join racing a reprice, withdrawal or product change leaves no free
// grant of what is no longer free, whether the join synced first or not.
func TestJoinRechecksFreeTerms(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	cfg := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader"}}, GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tags", Sellable: true}}}
	billing := &joinBilling{products: map[string]access.Product{}}
	gate, err := access.NewGate(access.GateConfig{Tenant: "policy", Kinds: cfg, Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg, Billing: billing, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	actor := access.Actor{ID: "reader"}
	paid := Terms{Amount: 5_000_000, Currency: "USD"}
	// group creates a free group offer on product p; synced or awaiting sync.
	group := func(t *testing.T, p string, synced bool) (string, access.Key) {
		t.Helper()
		id := contentref.NewID()
		key, _ := access.GroupKey("policy", id)
		billing.products[key.String()] = access.Product{ID: p, Key: key.String(), DisplayName: id}
		rev := 0
		if synced {
			rev = 1
		}
		if _, err := pool.Exec(ctx, `INSERT INTO `+table("content_groups")+` (tenant_id,id,kind,slug) VALUES ('policy',$1,'tag',$1)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO `+table("content_offers")+` (tenant_id,id,target_kind,target_id,terms,revision,synced_revision,product_id)
 VALUES ('policy',$1,'group',$2,'{"amount":0,"currency":"USD"}',1,$3,$4)`, contentref.NewID(), id, rev, p); err != nil {
			t.Fatal(err)
		}
		return id, key
	}
	reprice := func(id string) func() error {
		return func() error {
			_, err := s.Put(ctx, []Put{{Target: Target{Kind: GroupTarget, ID: id}, Terms: paid}})
			return err
		}
	}
	for _, c := range []struct {
		name   string
		synced bool
		race   func(id string) func() error
		want   error // nil: ErrNotJoinable
		free   bool  // the offer is free again once the race is done
	}{
		{name: "repriced during sync", race: reprice},
		{name: "repriced before grant", synced: true, race: reprice},
		{name: "withdrawn before grant", synced: true, race: func(id string) func() error {
			return func() error {
				_, err := s.Withdraw(ctx, []Target{{Kind: GroupTarget, ID: id}})
				return err
			}
		}},
		{name: "product recreated before grant", synced: true, free: true, race: func(id string) func() error {
			return func() error {
				if _, err := s.Withdraw(ctx, []Target{{Kind: GroupTarget, ID: id}}); err != nil {
					return err
				}
				_, err := s.Put(ctx, []Put{{Target: Target{Kind: GroupTarget, ID: id}}})
				return err
			}
		}},
		{name: "repriced and grant answer lost", synced: true, want: errLost, race: func(id string) func() error {
			return func() error {
				if err := reprice(id)(); err != nil {
					return err
				}
				return errLost
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			id, key := group(t, contentref.NewID(), c.synced)
			race := c.race(id)
			if c.synced {
				billing.beforeGrant = race
			} else {
				billing.beforeProducts = func() {
					if err := race(); err != nil {
						t.Error(err)
					}
				}
			}
			want := c.want
			if want == nil {
				want = ErrNotJoinable
			}
			if _, err := s.Join(ctx, actor, []string{id}); !errors.Is(err, want) {
				t.Fatalf("join: %v, want %v", err, want)
			}
			if slices.Contains(billing.keys, key.String()) {
				t.Fatal("a free grant of a group no longer free remains")
			}
			// A retry joins the offer as it now is.
			if _, err := s.Join(ctx, actor, []string{id}); c.free != (err == nil) || slices.Contains(billing.keys, key.String()) != c.free {
				t.Fatalf("retry: %v, held %v", err, slices.Contains(billing.keys, key.String()))
			}
		})
	}

	// A free join grants once; a retry, or a held membership, grants and
	// takes back nothing.
	id, key := group(t, contentref.NewID(), true)
	for i := range 2 {
		grants, revokes := billing.grants, billing.revokes
		out, err := s.Join(ctx, actor, []string{id})
		if err != nil || len(out) != 1 || !out[0].Member || !slices.Contains(billing.keys, key.String()) {
			t.Fatalf("free join: %+v %v", out, err)
		}
		if billing.grants-grants != 1-i || billing.revokes != revokes {
			t.Fatalf("join %d: %d grants, %d revokes", i, billing.grants-grants, billing.revokes-revokes)
		}
	}

	t.Run("failed batch preserves a concurrent successful join", func(t *testing.T) {
		a, aKey := group(t, contentref.NewID(), true)
		b, bKey := group(t, contentref.NewID(), true)
		billing.beforeGrant = func() error {
			if _, err := s.Join(ctx, actor, []string{a}); err != nil {
				return err
			}
			_, err := s.Withdraw(ctx, []Target{{Kind: GroupTarget, ID: b}})
			return err
		}
		if _, err := s.Join(ctx, actor, []string{a, b}); !errors.Is(err, ErrNotJoinable) {
			t.Fatalf("batch join: %v", err)
		}
		if !slices.Contains(billing.keys, aKey.String()) || slices.Contains(billing.keys, bKey.String()) {
			t.Fatal("rollback removed the successful join or retained the withdrawn offer")
		}
	})

	t.Run("cancelled caller still has bounded compensation", func(t *testing.T) {
		id, key := group(t, contentref.NewID(), true)
		joinCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		billing.beforeGrant = func() error {
			if err := reprice(id)(); err != nil {
				return err
			}
			cancel()
			return context.Canceled
		}
		billing.beforeRevoke = func(cleanup context.Context) error {
			deadline, ok := cleanup.Deadline()
			if !ok || time.Until(deadline) > joinCleanupTimeout || cleanup.Err() != nil {
				t.Fatalf("cleanup must survive caller cancellation with a bounded deadline: %v", cleanup.Err())
			}
			return nil
		}
		defer func() { billing.beforeRevoke = nil }()
		if _, err := s.Join(joinCtx, actor, []string{id}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled join: %v", err)
		}
		if slices.Contains(billing.keys, key.String()) {
			t.Fatal("cancelled join retained the repriced grant")
		}
	})

	t.Run("unresolved grant recovery is not an unavailable offer", func(t *testing.T) {
		id, _ := group(t, contentref.NewID(), true)
		billing.beforeGrant = func() error {
			if _, err := s.Withdraw(ctx, []Target{{Kind: GroupTarget, ID: id}}); err != nil {
				return err
			}
			return errLost
		}
		recoveryErr := errors.New("billing unavailable during replay")
		billing.replayErr = recoveryErr
		defer func() { billing.replayErr = nil }()
		_, err := s.Join(ctx, actor, []string{id})
		if !errors.Is(err, errLost) || !errors.Is(err, recoveryErr) {
			t.Fatalf("missing billing failure: %v", err)
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotJoinable) {
			t.Fatalf("unresolved grant recovery misclassified as an unavailable offer: %v", err)
		}
	})
}

var errLost = errors.New("grant answer lost")
