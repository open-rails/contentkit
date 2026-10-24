package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

// Only storage outcomes are controlled: ordering, revision guards, routing and
// retirement claims use the real PostgreSQL ledger. No object store is needed.
type fairnessStore struct {
	Store
	failed map[string]bool
	bodies map[string][]byte
	seen   map[string]int
}

func (*fairnessStore) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{ConditionalPut: true}, nil
}
func (s *fairnessStore) Get(_ context.Context, key string, o GetOptions) (io.ReadCloser, Object, error) {
	s.seen[key]++
	if s.failed[key] {
		return nil, Object{}, errors.New("persistent storage failure")
	}
	body, ok := s.bodies[key]
	if !ok {
		return nil, Object{}, ErrNotFound
	}
	if o.IfNoneMatch == "fixed" {
		return nil, Object{}, ErrNotModified
	}
	return io.NopCloser(bytes.NewReader(body)), Object{Key: key, ETag: "fixed", Size: int64(len(body))}, nil
}

type fairnessLocker struct{}

func (fairnessLocker) Lock(context.Context, string) (func(), error) { return func() {}, nil }

type fairnessFixture struct {
	t            *testing.T
	jobs         *Jobs
	pool         *pgxpool.Pool
	app, account *PGJournal
	store        *fairnessStore
	now          time.Time
}

func newFairnessFixture(t *testing.T) *fairnessFixture {
	t.Helper()
	f := &fairnessFixture{t: t, pool: pgtest.Pool(t, nil), now: time.Now().UTC().Truncate(time.Microsecond)}
	appSchema := pgtest.Schema(t, t.Context(), f.pool)
	accountSchema := pgtest.EmptySchema(t, t.Context(), f.pool)
	db := stdlib.OpenDBFromPool(f.pool)
	defer db.Close()
	if err := migrations.ApplyAccounts(t.Context(), db, accountSchema); err != nil {
		t.Fatal(err)
	}
	var err error
	f.app, err = NewPGJournal(f.pool, appSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.account, err = NewPGJournal(f.pool, accountSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(Config{Namespace: "fair-app", Kinds: []Kind{
		{Name: "post", Uploads: []Upload{{Path: "image", Types: []string{"image/png"}, MaxBytes: 1024}}},
		{Name: "user", Namespace: "fair-account", Uploads: []Upload{{Path: "avatar", Types: []string{"image/png"}, MaxBytes: 1024}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := RouteJournal(f.app, f.account, "fair-account")
	if err != nil {
		t.Fatal(err)
	}
	f.store = &fairnessStore{failed: map[string]bool{}, bodies: map[string][]byte{}, seen: map[string]int{}}
	f.jobs, err = NewJobs(JobsConfig{Store: f.store, Registry: reg, Locker: fairnessLocker{}, Journal: journal, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fairnessFixture) due(kind string, at time.Time, failed bool) Item {
	f.t.Helper()
	ref, err := f.jobs.cfg.Registry.Ref(kind, contentref.NewID())
	if err != nil {
		f.t.Fatal(err)
	}
	item, err := f.jobs.cfg.Registry.Item(ref)
	if err != nil {
		f.t.Fatal(err)
	}
	j := f.jobs.cfg.Journal.forTenant(ref.TenantID)
	_, err = f.pool.Exec(f.t.Context(), `INSERT INTO `+j.cleanup+`(tenant_id,folder_prefix,content_kind,due_at) VALUES($1,$2,$3,$4)`, ref.TenantID, item.Prefix(), kind, at)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store.failed[item.ManifestKey()] = failed
	return item
}
func (f *fairnessFixture) pending(item Item) bool {
	f.t.Helper()
	var exists bool
	j := f.jobs.cfg.Journal.forTenant(item.Ref().TenantID)
	if err := f.pool.QueryRow(f.t.Context(), `SELECT EXISTS(SELECT 1 FROM `+j.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2)`, item.Ref().TenantID, item.Prefix()).Scan(&exists); err != nil {
		f.t.Fatal(err)
	}
	return exists
}

func TestPurgeRetiredFairAcrossFoldersAndNamespaces(t *testing.T) {
	f := newFairnessFixture(t)
	const limit = 4
	var failed []Item
	for _, kind := range []string{"post", "user"} {
		for range 2*limit + 1 {
			failed = append(failed, f.due(kind, f.now.Add(-2*time.Hour), true))
		}
	}
	app := f.due("post", f.now.Add(-time.Hour), false)
	account := f.due("user", f.now.Add(-time.Hour), false)
	// More than two full failed batches in each namespace precede healthy work.
	// Retries are eligible on every new pass, but may not retake the front.
	for pass := 0; pass <= len(failed)/limit+2 && (f.pending(app) || f.pending(account)); pass++ {
		n, err := f.jobs.PurgeRetired(f.t.Context(), limit)
		if n < 1 || n > limit {
			t.Fatalf("attempted %d folders with limit%d", n, limit)
		}
		if pass == 0 && (n != limit || err == nil) {
			t.Fatalf("first all-failed batch=%d,%v", n, err)
		}
		f.now = f.now.Add(time.Second)
	}
	if f.pending(app) || f.pending(account) {
		t.Fatal("persistent failures starved healthy app/account folders")
	}
	for _, item := range failed {
		if !f.pending(item) {
			t.Fatal("rotation discarded failed deletion")
		}
	}
	// Failed rows are still retried by this same operation after healthy work.
	before := 0
	for _, n := range f.store.seen {
		before += n
	}
	n, err := f.jobs.PurgeRetired(f.t.Context(), limit)
	after := 0
	for _, v := range f.store.seen {
		after += v
	}
	if n != limit || err == nil || after <= before {
		t.Fatalf("failed obligations stopped retrying: attempts=%d err=%v", n, err)
	}
}

func TestPurgeRetiredLimitOneReachesAccounts(t *testing.T) {
	f := newFairnessFixture(t)
	failed := f.due("post", f.now.Add(-2*time.Hour), true)
	account := f.due("user", f.now.Add(-time.Hour), false)
	if n, err := f.jobs.PurgeRetired(f.t.Context(), 1); n != 1 || err == nil {
		t.Fatalf("failed app attempt: %d %v", n, err)
	}
	f.now = f.now.Add(time.Second)
	if n, err := f.jobs.PurgeRetired(f.t.Context(), 1); n != 1 || err != nil {
		t.Fatalf("later account attempt: %d %v", n, err)
	}
	if f.pending(account) || !f.pending(failed) {
		t.Fatal("one-item budget starved account or discarded app failure")
	}
}

func TestRetirementClaimDoesNotRewindPurgeRotation(t *testing.T) {
	f := newFairnessFixture(t)
	ref, err := f.jobs.cfg.Registry.Ref("post", contentref.NewID())
	if err != nil {
		t.Fatal(err)
	}
	item, _ := f.jobs.cfg.Registry.Item(ref)
	body, err := encodeManifest(&Manifest{Incarnation: uuid.NewString(), Files: []File{}})
	if err != nil {
		t.Fatal(err)
	}
	f.store.bodies[item.ManifestKey()] = body
	claim := func() {
		f.t.Helper()
		if err := pgx.BeginFunc(f.t.Context(), f.pool, func(tx pgx.Tx) error { return f.jobs.RetireItemsTx(f.t.Context(), tx, Deletion{Ref: ref}) }); err != nil {
			f.t.Fatal(err)
		}
	}
	claim()
	f.store.failed[item.ManifestKey()] = true
	if n, err := f.jobs.PurgeRetired(f.t.Context(), 1); n != 1 || err == nil {
		t.Fatalf("failed claim: %d %v", n, err)
	}
	var before, after time.Time
	var revisionBefore, revisionAfter uuid.UUID
	read := func(at *time.Time, revision *uuid.UUID) {
		f.t.Helper()
		if err := f.pool.QueryRow(f.t.Context(), `SELECT due_at,revision FROM `+f.app.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, ref.TenantID, item.Prefix()).Scan(at, revision); err != nil {
			f.t.Fatal(err)
		}
	}
	read(&before, &revisionBefore)
	f.store.failed[item.ManifestKey()] = false
	// Deliberately keep the same clock: re-claim must not lower due_at back to
	// the old cutoff or invalidate an active sweep's revision snapshot.
	claim()
	read(&after, &revisionAfter)
	if !before.Equal(after) || revisionBefore != revisionAfter {
		t.Fatalf("repeat claim rewound rotation: %s/%s -> %s/%s", before, revisionBefore, after, revisionAfter)
	}
}
