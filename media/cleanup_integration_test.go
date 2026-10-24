package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/migrations"
)

func cleanupTable(f *fixture) string {
	return pgx.Identifier{f.env.ContentSchema(), "content_media_cleanup"}.Sanitize()
}

func cleanupDueNow(t *testing.T, f *fixture, refs ...contentref.ContentRef) {
	t.Helper()
	for _, ref := range refs {
		item, _ := f.reg.Item(ref)
		if _, err := f.env.Pool().Exec(t.Context(), "UPDATE "+cleanupTable(f)+" SET due_at=now()-interval '1 minute' WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()); err != nil {
			t.Fatal(err)
		}
	}
}

func cleanupCount(t *testing.T, f *fixture, ref contentref.ContentRef) int {
	t.Helper()
	item, _ := f.reg.Item(ref)
	var count int
	if err := f.env.Pool().QueryRow(t.Context(), "SELECT count(*) FROM "+cleanupTable(f)+" WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// A committed edit and even an abandoned upload have durable cleanup state;
// repeated requests remain one folder, and a sweep may not discard a future
// temp-file deadline just because it found nothing deletable today.
func TestCleanupDueRetainsYoungTempAndAllocations(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.gallery(1, 1)
	item, _ := f.reg.Item(ref)
	for range 5 {
		if err := f.jobs.ScheduleSweep(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
	if got := cleanupCount(t, f, ref); got != 1 {
		t.Fatalf("repeated requests created %d cleanup records", got)
	}
	_, pending := f.upload(ref, "originals/abandoned.png", "image/png", png(2))
	temp, _ := item.Staged(pending)
	// Allocate without sending bytes yet: this must keep its own future pass.
	if _, err := f.ms.NewBlobs(t.Context(), ref, "private", [][]byte{make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	cleanupDueNow(t, f, ref)
	if n, err := f.jobs.PurgeRetired(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("first due pass: %d %v", n, err)
	}
	if !f.exists(temp) || cleanupCount(t, f, ref) != 1 {
		t.Fatal("young temp or its future cleanup was discarded")
	}
	// Run just after the private allocation's grace but before TempTTL.
	middle := time.Now().Add(25 * time.Hour)
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal,
		Now: func() time.Time { return middle }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.PurgeRetired(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("temp still young: %d %v", n, err)
	}
	if !f.exists(temp) || cleanupCount(t, f, ref) != 1 {
		t.Fatal("TempTTL deadline was lost after private grace elapsed")
	}
	if n, err := f.later().PurgeRetired(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("final due pass: %d %v", n, err)
	}
	if f.exists(temp) || cleanupCount(t, f, ref) != 1 {
		t.Fatal("expired upload survived or its bounded late-upload pass was lost")
	}
	last, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal,
		Now: func() time.Time { return time.Now().Add(20 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := last.PurgeRetired(t.Context(), 10); err != nil || n != 1 || cleanupCount(t, f, ref) != 0 {
		t.Fatalf("bounded late-upload pass did not finish: %d %v", n, err)
	}
}

type dueCleanupListStore struct {
	media.Store
	before func(string)
	fail   string
}

func TestCleanupDueIsBoundedAndIdleDoesNotListStorage(t *testing.T) {
	f := newFixture(t)
	listed := false
	jobs, err := media.NewJobs(media.JobsConfig{Store: dueCleanupListStore{Store: f.env.Store, before: func(string) { listed = true }},
		Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.PurgeRetired(t.Context(), 1); err != nil || n != 0 || listed {
		t.Fatalf("idle cleanup touched storage: %d %v listed=%v", n, err, listed)
	}
	refs := []contentref.ContentRef{f.ref("gallery", 1), f.ref("gallery", 2)}
	for _, ref := range refs {
		if _, err := f.ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
			m.Meta = map[string]any{"title": "bounded"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range refs {
		if err := f.jobs.ScheduleSweep(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
	cleanupDueNow(t, f, refs...)
	if n, err := jobs.PurgeRetired(t.Context(), 1); err != nil || n != 1 {
		t.Fatalf("bounded cleanup: %d %v", n, err)
	}
	if got := cleanupCount(t, f, refs[0]) + cleanupCount(t, f, refs[1]); got != 1 {
		t.Fatalf("cleanup limit left %d records, want one", got)
	}
}

func (s dueCleanupListStore) Get(ctx context.Context, key string, opts media.GetOptions) (io.ReadCloser, media.Object, error) {
	if s.before != nil {
		s.before(key)
	}
	if s.fail != "" && strings.HasPrefix(key, s.fail) {
		return nil, media.Object{}, errors.New("injected folder read failure")
	}
	return s.Store.Get(ctx, key, opts)
}

func (s dueCleanupListStore) List(ctx context.Context, prefix string) iter.Seq2[media.Object, error] {
	if s.before != nil {
		s.before(prefix)
	}
	if prefix == s.fail {
		return func(yield func(media.Object, error) bool) {
			yield(media.Object{}, errors.New("injected folder read failure"))
		}
	}
	return s.Store.List(ctx, prefix)
}

func TestCleanupDueKeepsConcurrentRequestAndIsolatesFailures(t *testing.T) {
	f := newFixture(t)
	one, two := f.ref("gallery", 1), f.ref("gallery", 2)
	for _, ref := range []contentref.ContentRef{one, two} {
		if _, err := f.ms.Edit(t.Context(), ref, func(m *media.Manifest) error { m.Meta = map[string]any{"empty": true}; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []contentref.ContentRef{one, two} {
		if err := f.jobs.ScheduleSweep(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := f.reg.Item(one)
	var once sync.Once
	store := dueCleanupListStore{Store: f.env.Store, fail: first.Prefix(), before: func(prefix string) {
		if !strings.HasPrefix(prefix, first.Prefix()) {
			once.Do(func() {
				if err := f.jobs.ScheduleSweep(t.Context(), two); err != nil {
					t.Error(err)
				}
			})
		}
	}}
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal})
	if err != nil {
		t.Fatal(err)
	}
	cleanupDueNow(t, f, one, two)
	if n, err := jobs.PurgeRetired(t.Context(), 10); n != 2 || err == nil || !strings.Contains(err.Error(), "injected folder read failure") {
		t.Fatalf("isolated pass: %d %v", n, err)
	}
	if cleanupCount(t, f, one) != 1 || cleanupCount(t, f, two) != 1 {
		t.Fatal("failed folder or concurrent request lost its obligation")
	}
	cleanupDueNow(t, f, two)
	if n, err := f.jobs.PurgeRetired(t.Context(), 10); n != 1 || err != nil {
		t.Fatalf("unaffected folder did not progress: %d %v", n, err)
	}
	if cleanupCount(t, f, two) != 0 {
		t.Fatal("unchanged completed request was not acknowledged")
	}
}

type gatedCleanupLocker struct {
	media.Locker
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (l *gatedCleanupLocker) Lock(ctx context.Context, key string) (func(), error) {
	l.once.Do(func() {
		close(l.entered)
		select {
		case <-l.resume:
		case <-ctx.Done():
		}
	})
	return l.Locker.Lock(ctx, key)
}

func TestCleanupAcknowledgmentCannotEraseRecreatedRow(t *testing.T) {
	f := newFixture(t)
	ref := f.ref("gallery", 1)
	if _, err := f.ms.Edit(t.Context(), ref, func(m *media.Manifest) error { m.Meta = map[string]any{"title": "first"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.jobs.ScheduleSweep(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	cleanupDueNow(t, f, ref)
	gate := &gatedCleanupLocker{Locker: s3test.Locker(t, f.env.Store), entered: make(chan struct{}), resume: make(chan struct{})}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: gate, Journal: f.journal})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := jobs.PurgeRetired(ctx, 1); done <- err }()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The second cleaner has read the old token. Complete and delete that row,
	// then create a new obligation before allowing its stale ACK to proceed.
	if _, err := f.jobs.PurgeRetired(ctx, 1); err != nil || cleanupCount(t, f, ref) != 0 {
		t.Fatalf("first cleaner did not finish: %v", err)
	}
	if _, err := f.ms.Edit(ctx, ref, func(m *media.Manifest) error { m.Meta["title"] = "new request"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.jobs.ScheduleSweep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	close(gate.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cleanupCount(t, f, ref) != 1 {
		t.Fatal("stale cleanup acknowledgment erased a recreated obligation")
	}
}

func TestCleanupRetiresAbsentAllocationAndCatchesLateUpload(t *testing.T) {
	f := newFixture(t)
	ref := f.ref("gallery", 1)
	names, err := f.ms.NewBlobs(t.Context(), ref, "private", [][]byte{make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	key, _ := item.Blob(names[0])
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal,
		Now: func() time.Time { return time.Now().Add(25 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.PurgeRetired(t.Context(), 10); n != 1 || err != nil {
		t.Fatalf("absent allocation retirement: %d %v", n, err)
	}
	var retired bool
	if err := f.env.Pool().QueryRow(t.Context(), "SELECT retired_at IS NOT NULL FROM "+pgx.Identifier{f.env.ContentSchema(), "content_media_allocations"}.Sanitize()+" WHERE tenant_id=$1 AND object_key=$2", ref.TenantID, key).Scan(&retired); err != nil || !retired {
		t.Fatalf("absent allocation stayed writable: %v %v", retired, err)
	}
	if cleanupCount(t, f, ref) != 1 {
		t.Fatal("retirement lost its late-upload cleanup obligation")
	}
	f.object(key, "late bytes")
	if n, err := f.later().PurgeRetired(t.Context(), 10); n != 1 || err != nil {
		t.Fatalf("late upload cleanup: %d %v", n, err)
	}
	if f.exists(key) || cleanupCount(t, f, ref) != 0 {
		t.Fatal("late upload survived or cleanup never completed")
	}
}

// Migration moves timers and their exact final-delete targets atomically;
// missing a cancellation must roll back the ledger writes too.
func TestCleanupMigrationPreservesDeadlinesAndRollsBack(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.gallery(1, 1)
	item, _ := f.reg.Item(ref)
	pool := pgtest.Pool(t, func(cfg *pgxpool.Config) { cfg.MaxConns = 1 })
	journal, err := media.NewPGJournal(pool, f.env.ContentSchema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, schema); err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	insert := func(kind string, args any, due time.Time) int64 {
		t.Helper()
		body, _ := json.Marshal(args)
		var id int64
		if err := pool.QueryRow(t.Context(), "INSERT INTO "+table+" (kind,args,queue,max_attempts,state,scheduled_at) VALUES ($1,$2,'contentkit_media',3,'scheduled',$3) RETURNING id", kind, body, due).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before, after := time.Now().UTC().Add(24*time.Hour).Truncate(time.Microsecond), time.Now().UTC().Add(25*time.Hour).Truncate(time.Microsecond)
	if err := f.jobs.Purge(t.Context(), media.Deletion{Ref: ref, OperationID: "legacy-cleanup-migration"}); err != nil {
		t.Fatal(err)
	}
	var operation uuid.UUID
	if err := pool.QueryRow(t.Context(), "SELECT operation_id FROM "+pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()+" WHERE tenant_id=$1 AND folder_prefix=$2 AND effects->>'deletion'='true' ORDER BY created_at DESC LIMIT 1", ref.TenantID, item.Prefix()).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	insert("contentkit_media_sweep", map[string]any{"prefix": item.Prefix()}, before)
	final := insert("contentkit_media_delete_folder", map[string]any{"prefix": item.Prefix(), "final": true, "operation": operation.String()}, after)
	insert("contentkit_media_sweep_pass", map[string]any{}, before)
	orphan := insert("contentkit_media_orphan_sweep", map[string]any{}, before)
	immediate := insert("contentkit_media_delete_folder", map[string]any{"prefix": item.Prefix(), "incarnation": uuid.NewString()}, before)
	foreign := insert("contentkit_media_sweep", map[string]any{"prefix": "another-app/gallery/" + cid(1) + "/"}, before)
	if _, err := pool.Exec(t.Context(), "UPDATE "+cleanupTable(f)+" SET due_at='infinity' WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()); err != nil {
		t.Fatal(err)
	}
	// A syntactically valid UUID without a journal receipt must not consume a
	// timer and replace it with cleanup that can never recover its targets.
	invalid := insert("contentkit_media_delete_folder", map[string]any{"prefix": item.Prefix(), "final": true, "operation": uuid.NewString()}, after)
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, journal, schema, 0); err == nil {
		t.Fatal("migration accepted a missing canonical deletion")
	}
	var uninitialized bool
	if err := pool.QueryRow(t.Context(), "SELECT due_at='infinity'::timestamptz FROM "+cleanupTable(f)+" WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()).Scan(&uninitialized); err != nil || !uninitialized {
		t.Fatalf("failed migration partially initialized bootstrap: %v %v", uninitialized, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE "+table+" SET state='cancelled', finalized_at=now() WHERE id=$1", invalid); err != nil {
		t.Fatal(err)
	}
	if err := media.ValidateCleanupMigration(t.Context(), pool, f.reg, journal, schema); err == nil {
		t.Fatal("startup accepted unmigrated timers")
	}
	// Simulate a running old worker. No preceding row may partially migrate.
	if _, err := pool.Exec(t.Context(), "UPDATE "+table+" SET state='running' WHERE id=$1", final); err != nil {
		t.Fatal(err)
	}
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, journal, schema, 0); err == nil {
		t.Fatal("migration accepted a running old cleanup worker")
	}
	var cancelled int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE state='cancelled' AND id<>$1", invalid).Scan(&cancelled); err != nil || cancelled != 0 {
		t.Fatalf("partial migration was not rolled back: %d %v", cancelled, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE "+table+" SET state='scheduled' WHERE id=$1", final); err != nil {
		t.Fatal(err)
	}
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, journal, schema, 0); err != nil {
		t.Fatal(err)
	}
	if err := media.ValidateCleanupMigration(t.Context(), pool, f.reg, journal, schema); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var operations []uuid.UUID
	if err := pool.QueryRow(t.Context(), "SELECT due_at,deletion_operations FROM "+cleanupTable(f)+" WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()).Scan(&due, &operations); err != nil {
		t.Fatal(err)
	}
	if due.Before(after) || !slices.Contains(operations, operation) {
		t.Fatalf("lost deadline/targets: %s %v", due, operations)
	}
	var state string
	if err := pool.QueryRow(t.Context(), "SELECT state FROM "+table+" WHERE id=$1", orphan).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("obsolete orphan crawler was not cancelled: %s %v", state, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state FROM "+table+" WHERE id=$1", immediate).Scan(&state); err != nil || state != "scheduled" {
		t.Fatalf("immediate deletion was changed: %s %v", state, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state FROM "+table+" WHERE id=$1", foreign).Scan(&state); err != nil || state != "scheduled" {
		t.Fatalf("another app's cleanup was changed: %s %v", state, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE "+cleanupTable(f)+" SET due_at=now()-interval '1 day' WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()); err != nil {
		t.Fatal(err)
	}
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, journal, schema, 0); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT due_at < now() FROM "+cleanupTable(f)+" WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()).Scan(&uninitialized); err != nil || !uninitialized {
		t.Fatalf("migration rerun reset an already due obligation: %v %v", uninitialized, err)
	}
	if _, err := pool.Exec(t.Context(), "DELETE FROM "+cleanupTable(f)+" WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()); err != nil {
		t.Fatal(err)
	}
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, journal, schema, 0); err != nil || cleanupCount(t, f, ref) != 0 {
		t.Fatalf("migration rerun reseeded completed cleanup: %v", err)
	}
}

func TestCleanupBootstrapPreservesConfiguredAllocationGrace(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	allocation := f.blob(ref, []byte("unfinished private output"), "image/png")
	item, _ := f.reg.Item(ref)
	public, _ := item.Blob(allocation)
	pool := f.env.Pool()
	if _, err := pool.Exec(t.Context(), "UPDATE "+cleanupTable(f)+" SET due_at='infinity' WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()); err != nil {
		t.Fatal(err)
	}
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := media.ValidateCleanupMigration(t.Context(), pool, f.reg, f.journal, schema); err == nil {
		t.Fatal("startup accepted an unresolved cleanup bootstrap")
	}
	const grace = 48 * time.Hour
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, f.journal, schema, grace); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Add(24 * time.Hour)
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal,
		Grace: grace, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.PurgeRetired(t.Context(), 10); n != 0 || err != nil || !f.exists(public) {
		t.Fatalf("bootstrap shortened custom allocation grace: %d %v", n, err)
	}
	clock = time.Now().Add(49 * time.Hour)
	if n, err := jobs.PurgeRetired(t.Context(), 10); n != 1 || err != nil || f.exists(public) {
		t.Fatalf("bootstrap never collected expired allocated output: %d %v", n, err)
	}
}

// Shared avatars must use the tenant's account schema, while a gallery's
// cleanup remains in its app catalog. Processing one must not drain the other.
func TestCleanupUsesAccountJournalRoute(t *testing.T) {
	f := newFixture(t)
	pool := f.env.Pool()
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	if err := migrations.ApplyAccounts(t.Context(), db, schema); err != nil {
		t.Fatal(err)
	}
	account, err := media.NewPGJournal(pool, schema, f.q)
	if err != nil {
		t.Fatal(err)
	}
	routed, err := media.RouteJournal(f.journal, account, f.shared)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: routed, Processes: f.q})
	if err != nil {
		t.Fatal(err)
	}
	ref := f.ref("user", 7)
	if _, err := jobs.Manifests().Edit(t.Context(), ref, func(m *media.Manifest) error {
		m.Meta = map[string]any{"account": true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ScheduleSweep(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	var count int
	accountTable := pgx.Identifier{schema, "content_media_cleanup"}.Sanitize()
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+accountTable+" WHERE tenant_id=$1 AND folder_prefix=$2", ref.TenantID, item.Prefix()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("shared account cleanup missing: %d %v", count, err)
	}
	if cleanupCount(t, f, ref) != 0 {
		t.Fatal("account cleanup leaked into app catalog")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE "+accountTable+" SET due_at=now()-interval '1 minute' WHERE tenant_id=$1", ref.TenantID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.jobs.PurgeRetired(t.Context(), 10); err != nil || n != 0 {
		t.Fatalf("app journal processed shared cleanup without route: %d %v", n, err)
	}
	if n, err := jobs.PurgeRetired(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("routed shared cleanup: %d %v", n, err)
	}
}
