package content

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
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
	if opts.Auth == nil {
		opts.Auth = testauth.Auth{Authz: allowAll{}}
	}
	if opts.Resolver == nil {
		opts.Resolver = &fakeResolver{}
	}
	if reflect.ValueOf(opts.Limits).IsZero() {
		opts.Limits.Disabled = true // tests opt in to rate limits by setting any field
	}
	rt, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("New runtime: %v", err)
	}
	return rt, pool
}

// as is a as the route gate builds it over rt's test Auth: its Verified
// answers Can as the runtime's Authorizer does.
func (rt *Runtime) as(a access.Actor) access.Actor {
	ta, _ := rt.guard.Auth.(testauth.Auth)
	return testauth.Actor(a, ta.Authz)
}

// cid is the nth deterministic content id: a canonical UUIDv7, ordered by n.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

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

// setCanonical is set for a visible, accessible route id the resolver maps to
// canonical, keeping a requested version.
func (f *fakeResolver) setCanonical(kind, id, canonical string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]access.Resolution{}
	}
	f.entries[kind+":"+id] = access.Resolution{Visible: true, Accessible: true, Ref: contentref.New(testTenant, kind, canonical)}
}

// setOwned is set for a visible, accessible target owned by owner.
func (f *fakeResolver) setOwned(kind, id, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]access.Resolution{}
	}
	f.entries[kind+":"+id] = access.Resolution{Visible: true, Accessible: true, Owner: owner}
}

func (f *fakeResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if res, ok := f.entries[r.ContentKind+":"+r.ContentID]; ok {
			if res.Ref.ContentID != "" && r.Version() != "" {
				res.Ref = res.Ref.WithVersion(r.Version())
			}
			out[r.Key()] = res
		}
	}
	return out, nil
}

// testMedia records media lifecycle requests and builds inline URLs.
type testMedia struct {
	mu      sync.Mutex
	deleted []string
	exposed []string
	origin  string
	gens    map[string]string // published image name -> its generation
	lookups int
	// avatarLookups counts Avatars calls.
	avatarLookups int
}

// publish gives the image name a current public file at a new generation.
func (m *testMedia) publish(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gens == nil {
		m.gens = map[string]string{}
	}
	m.gens[name] = uuid.NewString()
}

func (m *testMedia) unpublish(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.gens, name)
}

// url is the published file of name in ref's folder.
func (m *testMedia) url(ref contentref.ContentRef, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.urlLocked(ref, name)
}

func (m *testMedia) urlLocked(ref contentref.ContentRef, name string) string {
	origin := m.origin
	if origin == "" {
		origin = "https://media.test"
	}
	return origin + "/" + ref.TenantID + "/" + ref.ContentKind + "/" + ref.ContentID + "/public/" + name + "-" + m.gens[name] + ".webp"
}

// Images answers like the publications projection: a published name has one
// image of two renditions.
func (m *testMedia) Images(_ context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	out := make([][]media.PublicImage, len(qs))
	for i, q := range qs {
		if _, ok := m.gens[q.Name]; ok {
			u := m.urlLocked(q.Ref, q.Name)
			out[i] = []media.PublicImage{{From: q.Name + ".png", Preset: "inline", Renditions: []media.PublicRendition{
				{URL: strings.Replace(u, ".webp", "-small.webp", 1), W: 400}, {URL: u, W: 800}}}}
		}
	}
	return out, nil
}

// Avatars answers like the projection: an id with a published "avatar-{id}"
// has its avatar in two renditions, every other none.
func (m *testMedia) Avatars(_ context.Context, ids ...string) ([]media.PublicImage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.avatarLookups++
	out := make([]media.PublicImage, len(ids))
	for i, id := range ids {
		out[i] = media.PublicImage{Preset: "avatar"}
		if gen, ok := m.gens["avatar-"+id]; ok {
			u := "https://media.test/accounts/user/" + id + "/public/avatar-" + gen
			out[i].Renditions = []media.PublicRendition{{URL: u + "-64.webp", W: 64, H: 64}, {URL: u + "-128.webp", W: 128, H: 128}}
		}
	}
	return out, nil
}

func (m *testMedia) lookupCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookups
}

func (m *testMedia) DeleteItemsTx(_ context.Context, tx pgx.Tx, items ...media.Deletion) error {
	for _, d := range items {
		m.mu.Lock()
		m.deleted = append(m.deleted, d.Ref.String())
		m.mu.Unlock()
	}
	return nil
}

func (m *testMedia) ExposeTx(_ context.Context, _ pgx.Tx, refs ...contentref.ContentRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ref := range refs {
		m.exposed = append(m.exposed, ref.String())
	}
	return nil
}

func (m *testMedia) options() *Media { return &Media{Images: m, Folders: m} }

func (m *testMedia) deletions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

// favErr adapts favorites add/remove's (reference, error) return for error-only assertions.
func favErr(_ contentref.ContentRef, err error) error { return err }

// reactErr adapts react's (reference, error) return for error-only assertions.
func reactErr(_ contentref.ContentRef, err error) error { return err }

type processorFunc func(context.Context, string) (string, error)

func (f processorFunc) Sanitize(ctx context.Context, raw string) (string, error) {
	return f(ctx, raw)
}

// doJSON issues a request as actor and returns the recorder.
func doJSON(t *testing.T, h http.Handler, actor access.Actor, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := testauth.As(httptest.NewRequest(method, target, &buf), actor)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ptr[T any](v T) *T { return &v }
