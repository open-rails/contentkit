package content

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
)

// testTenant is the tenant every test runtime is pinned to unless it says otherwise.
const testTenant = "hostapp"

// newTestRuntime provisions a disposable host schema on CONTENTKIT_TEST_URL
// (skipping without it), applies the social lineage the way a host migrate
// step does, and builds a Runtime against the given (usually fake) ports.
func newTestRuntime(t *testing.T, opts Options) (*Runtime, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	if opts.Schema == "" {
		opts.Schema = pgtest.EmptySchema(t, ctx, pool)
		db := stdlib.OpenDBFromPool(pool)
		if err := Migrate(ctx, db, opts.Schema); err != nil {
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

func withActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

func (*fakeIdentity) Actor(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(Actor)
	return a, ok
}

// allowAll authorizes every perm; denyAll denies.
type allowAll struct{}

func (allowAll) Can(context.Context, Actor, string) (bool, error) { return true, nil }

type denyAll struct{}

func (denyAll) Can(context.Context, Actor, string) (bool, error) { return false, nil }

// fakeResolver answers from an in-memory map keyed by "kind:id"; missing =>
// not found. It ignores the tenant and keeps the caller's reference.
type fakeResolver struct {
	mu      sync.Mutex
	entries map[string]Resolution
}

func (f *fakeResolver) set(kind, id string, visible, accessible bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]Resolution{}
	}
	f.entries[kind+":"+id] = Resolution{Visible: visible, Accessible: accessible}
}

func (f *fakeResolver) Resolve(_ context.Context, r contentref.ContentRef, _ Actor) (Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res, ok := f.entries[r.ContentKind+":"+r.ContentID]
	if !ok {
		return Resolution{}, ErrNotFound
	}
	return res, nil
}

// fakeMedia is an in-memory MediaStore capturing uploads for assertions.
type fakeMedia struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func (f *fakeMedia) Put(_ context.Context, key string, data []byte, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = data
	return "https://cdn.test/" + key, nil
}

// DeleteByURL mirrors s3Store's optional mediaURLDeleter.
func (f *fakeMedia) DeleteByURL(_ context.Context, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := strings.CutPrefix(url, "https://cdn.test/")
	if !ok {
		return nil
	}
	delete(f.puts, key)
	return nil
}

func (f *fakeMedia) stored(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.puts[key]
	return d, ok
}

func (f *fakeMedia) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// reactErr adapts react's (ref, error) return for error-only test assertions.
func reactErr(_ contentref.ContentRef, err error) error { return err }
