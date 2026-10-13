package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
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
	if opts.Kinds.Kinds == nil {
		opts.Kinds = testKinds("gallery")
	}
	if reflect.ValueOf(opts.Limits).IsZero() {
		opts.Limits.Disabled = true // tests opt in to rate limits by setting any field
	}
	rt, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("New runtime: %v", err)
	}
	if fr, ok := opts.Resolver.(*fakeResolver); ok {
		fr.storeTargets(t, rt)
	}
	return rt, pool
}

// as is a as the route gate builds it over rt's test Auth: its Verified
// answers Can as the runtime's Authorizer does.
func (rt *Runtime) as(a access.Actor) access.Actor {
	ta, _ := rt.guard.Auth.(testauth.Auth)
	return testauth.Actor(a, ta.Authz)
}

// testKinds is a content model with the named content kinds; "tag" is a
// group kind.
func testKinds(names ...string) kinds.Config {
	c := kinds.Config{Languages: []string{"en", "ja"}, Kinds: map[string]kinds.Kind{}, GroupKinds: map[string]kinds.GroupKind{}}
	for _, n := range names {
		if n == "tag" {
			c.GroupKinds[n] = kinds.GroupKind{Route: "tags"}
			continue
		}
		c.Kinds[n] = kinds.Kind{Route: strings.ReplaceAll(n, "_", "-"), Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}}
	}
	if len(c.Kinds) == 0 {
		c.Kinds["gallery"] = kinds.Kind{Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}}
	}
	return c
}

// storeTarget stores a target of kind (a content item, or a group of a
// group kind) under id, as the catalog or groups would.
func storeTarget(t *testing.T, rt *Runtime, kind, id string) {
	t.Helper()
	typ, ok := rt.targetType(kind)
	if !ok {
		return
	}
	q := `INSERT INTO ` + rt.store.t.items + ` (tenant_id, id, kind) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`
	if typ == TargetGroup {
		q = `INSERT INTO ` + rt.store.t.groups + ` (tenant_id, id, kind, slug) VALUES ($1, $2, $3, $2) ON CONFLICT DO NOTHING`
	}
	if _, err := rt.store.pool.Exec(context.Background(), q, rt.tenant, id, kind); err != nil {
		t.Errorf("store target: %v", err)
	}
}

// favorited reads whether user favorited r.
func favorited(t *testing.T, rt *Runtime, user string, r contentref.ContentRef) bool {
	t.Helper()
	var on bool
	if err := rt.store.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM `+rt.store.t.favorites+` WHERE `+keyPred(1)+` AND user_id = $5 AND value = 1)`,
		append(keyArgs(r.Key()), user)...).Scan(&on); err != nil {
		t.Fatalf("favorited: %v", err)
	}
	return on
}

// reactionOf reads a's reaction to r: 0 for none.
func reactionOf(t *testing.T, rt *Runtime, a access.Actor, r contentref.ContentRef) int16 {
	t.Helper()
	user, ip, _ := reactionKey(a)
	var v int16
	err := rt.store.pool.QueryRow(context.Background(), `SELECT value FROM `+rt.store.t.reactions+` WHERE `+keyPred(1)+` AND `+actorPred(user, 5),
		append(keyArgs(r.Key()), actorArg(user, ip))...).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reactionOf: %v", err)
	}
	return v
}

// favoriteIDs lists the ids user favorited, newest first.
func favoriteIDs(t *testing.T, rt *Runtime, user string) []string {
	t.Helper()
	rows, err := rt.store.pool.Query(context.Background(), `SELECT content_id FROM `+rt.store.t.favorites+` WHERE tenant_id = $1 AND user_id = $2 AND value = 1
		ORDER BY created_at DESC, content_id DESC`, rt.tenant, user)
	if err != nil {
		t.Fatalf("favoriteIDs: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("favoriteIDs: %v", err)
	}
	return ids
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
	entries map[string]resolve.Resolution
	calls   int
	// stored stores each target set as a row the routes find it by.
	stored func(kind, id string)
}

func (f *fakeResolver) set(kind, id string, visible, accessible bool) {
	f.put(kind, id, resolve.Resolution{Visible: visible, Accessible: accessible})
}

func (f *fakeResolver) put(kind, id string, res resolve.Resolution) {
	f.mu.Lock()
	if f.entries == nil {
		f.entries = map[string]resolve.Resolution{}
	}
	f.entries[kind+":"+id] = res
	stored := f.stored
	f.mu.Unlock()
	if stored != nil {
		stored(kind, id)
	}
}

// storeTargets has every target res holds, and every one set later, stored
// as rows of rt for the routes' kind lookup.
func (f *fakeResolver) storeTargets(t *testing.T, rt *Runtime) {
	f.mu.Lock()
	f.stored = func(kind, id string) { storeTarget(t, rt, kind, id) }
	var keys []string
	for k := range f.entries {
		keys = append(keys, k)
	}
	f.mu.Unlock()
	for _, k := range keys {
		kind, id, _ := strings.Cut(k, ":")
		storeTarget(t, rt, kind, id)
	}
}

// setCanonical is set for a visible, accessible route id the resolver maps to
// canonical, keeping a requested version.
func (f *fakeResolver) setCanonical(kind, id, canonical string) {
	f.put(kind, id, resolve.Resolution{Visible: true, Accessible: true, Ref: contentref.New(testTenant, kind, canonical)})
	// The canonical reference resolves too, as a real resolver's does.
	f.put(kind, canonical, resolve.Resolution{Visible: true, Accessible: true})
}

// setOwned is set for a visible, accessible target owned by owner.
func (f *fakeResolver) setOwned(kind, id, owner string) {
	f.put(kind, id, resolve.Resolution{Visible: true, Accessible: true, Owner: owner})
}

func (f *fakeResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := map[contentref.ContentKey]resolve.Resolution{}
	for _, r := range refs {
		if res, ok := f.entries[r.ContentKind+":"+r.ContentID]; ok {
			if res.Ref.ContentID != "" {
				res.Ref = contentref.New(r.TenantID, res.Ref.ContentKind, res.Ref.ContentID).WithVersion(r.Version())
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
