package content

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/migrations"
)

// testTenant is the tenant every test runtime is pinned to unless it says otherwise.
const testTenant = "hostapp"

// newTestRuntime provisions a disposable host schema on CONTENTKIT_TEST_URL
// (skipping without it), applies the ContentKit baseline the way a host migrate
// step does, and builds a Runtime against the given (usually fake) ports.
func newTestRuntime(t *testing.T, opts Options) (*Runtime, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	if opts.Schema == "" {
		opts.Schema = pgtest.EmptySchema(t, ctx, pool)
		db := stdlib.OpenDBFromPool(pool)
		pgtest.EnsureExtensions(t, ctx, pool)
		if err := migrations.ApplyPostgres(ctx, db, opts.Schema); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		_ = db.Close()
	}
	opts.Pool = pool
	if opts.Tenant == "" {
		opts.Tenant = testTenant
	}
	if opts.Identity == nil {
		opts.Identity = &fakeIdentity{}
	}
	if opts.Authz == nil {
		opts.Authz = allowAll{}
	}
	if opts.Resolver == nil {
		opts.Resolver = &fakeResolver{}
	}
	rt, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("New runtime: %v", err)
	}
	return rt, pool
}

// ref builds a work reference of the test tenant.
func ref(kind, id string) contentref.ContentRef { return contentref.New(testTenant, kind, id) }

// countsOf reads one reference's rollup (zero value when absent).
func countsOf(t *testing.T, rt *Runtime, r contentref.ContentRef) Counts {
	t.Helper()
	m, err := rt.Counts(context.Background(), []contentref.ContentRef{r})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	return m[r.Key()]
}

// --- fake ports ---

// fakeIdentity returns the actor set per-request via withActor on context.
type fakeIdentity struct{}

type actorCtxKey struct{}

func withActor(ctx context.Context, a access.Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

func (*fakeIdentity) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(access.Actor)
	return a, ok
}

// allowAll authorizes every perm; denyAll denies.
type allowAll struct{}

func (allowAll) Can(context.Context, access.Actor, string) (bool, error) { return true, nil }

type denyAll struct{}

func (denyAll) Can(context.Context, access.Actor, string) (bool, error) { return false, nil }

// fakeResolver answers from an in-memory map keyed by "kind:id"; missing refs
// are omitted. It ignores the tenant, keeps the caller's reference and counts
// calls.
type fakeResolver struct {
	mu      sync.Mutex
	entries map[string]access.Resolution
	calls   int
}

func (f *fakeResolver) set(kind, id string, visible, accessible bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]access.Resolution{}
	}
	f.entries[kind+":"+id] = access.Resolution{Visible: visible, Accessible: accessible}
}

func (f *fakeResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if res, ok := f.entries[r.ContentKind+":"+r.ContentID]; ok {
			out[r.Key()] = res
		}
	}
	return out, nil
}

// testMedia is the Media port pair: URLs as media.Reader builds them, and the
// folder deletions requested inside committed transactions.
type testMedia struct {
	mu      sync.Mutex
	deleted []string
}

func (*testMedia) PublicURL(ref contentref.ContentRef, name string) (string, error) {
	return "https://media.test/" + ref.TenantID + "/" + ref.ContentKind + "/" + ref.ContentID + "/public/" + name + ".webp", nil
}

func (m *testMedia) DeleteItemsTx(_ context.Context, tx pgx.Tx, items ...media.Deletion) error {
	for _, d := range items {
		m.mu.Lock()
		m.deleted = append(m.deleted, d.Ref.String())
		m.mu.Unlock()
	}
	return nil
}

func (m *testMedia) options() *Media { return &Media{URLs: m, Folders: m} }

func (m *testMedia) deletions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

// reactErr adapts react's (reference, error) return for error-only assertions.
func reactErr(_ contentref.ContentRef, err error) error { return err }
