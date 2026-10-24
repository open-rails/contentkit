package versions_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	catalogpkg "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/workqueue"
)

type purgeAuthority struct{}

func (purgeAuthority) Can(context.Context, access.Actor, string) (bool, error) { return true, nil }
func (purgeAuthority) CanIn(context.Context, access.Actor, auth.Scope, string) (bool, error) {
	return true, nil
}

type purgeStorage struct {
	media.Store
	failDelete     atomic.Bool
	failPrefix     string
	ambiguousFence atomic.Bool
	fenceOnce      sync.Once
	beforeFence    func()
}

func (s *purgeStorage) Delete(ctx context.Context, key string) error {
	if s.failDelete.Load() && strings.HasPrefix(key, s.failPrefix) {
		return errors.New("injected S3 deletion failure")
	}
	return s.Store.Delete(ctx, key)
}
func (s *purgeStorage) Put(ctx context.Context, key string, body io.Reader, size int64, options media.PutOptions) (media.Object, error) {
	if strings.HasSuffix(key, layout.ErasureFenceName) && s.beforeFence != nil {
		s.fenceOnce.Do(s.beforeFence)
	}
	object, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil && strings.HasSuffix(key, layout.ErasureFenceName) && s.ambiguousFence.Swap(false) {
		return object, errors.New("injected lost fence PUT response")
	}
	return object, err
}

type purgeFixture struct {
	t            *testing.T
	env          *s3test.Env
	store        *purgeStorage
	catalog      *catalogpkg.Store
	versions     *versions.Service
	jobs         *media.Jobs
	manifests    *media.Manifests
	uploads      *media.Uploads
	registry     *media.Registry
	actor        access.Actor
	schema, host string
	clock        atomic.Int64
}

// No media worker or periodic reaper runs in this fixture. Every physical
// deletion must therefore be completed by the explicit catalog purge call.
func newPurgeFixture(t *testing.T) *purgeFixture {
	t.Helper()
	env := s3test.Open(t)
	if !env.Caps().ConditionalPut {
		t.Skip("conditional PUT required")
	}
	f := &purgeFixture{t: t, env: env, store: &purgeStorage{Store: env.Store}, schema: env.ContentSchema()}
	f.clock.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	now := func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	cfg := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}}}}
	authn := testauth.Auth{Authz: purgeAuthority{}}
	f.actor = testauth.Actor(access.Actor{ID: contentref.NewID()}, purgeAuthority{})
	var err error
	f.catalog, err = catalogpkg.New(t.Context(), catalogpkg.Options{Pool: env.Pool(), Schema: f.schema, Tenant: env.Tenant, Kinds: cfg, Auth: authn, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.catalog.Close)
	f.versions, err = versions.New(versions.Options{Tenant: env.Tenant, Schema: f.schema, Pool: env.Pool(), Kinds: cfg, Auth: authn, Resolver: f.catalog.Resolver(), Facets: f.catalog, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	f.registry, err = media.NewRegistry(media.Config{Namespace: env.Tenant, Hooks: f.versions.Hooks(), Kinds: []media.Kind{{Name: "gallery", Versions: true, KeepOriginals: true,
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Private: []media.Private{{Name: "read", From: "pages/{name}", To: "read/{name}.webp", Image: &media.Image{Width: 16}}},
		Publish: &media.Publish{Pages: "pages/{name}", Renditions: []string{"read"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	workerSchema := pgtest.EmptySchema(t, t.Context(), env.Pool())
	if err = workqueue.Migrate(t.Context(), env.Pool(), workerSchema); err != nil {
		t.Fatal(err)
	}
	queue, err := workqueue.New(env.Pool(), f.registry, workerSchema)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := media.NewPGJournal(env.Pool(), f.schema, queue)
	if err != nil {
		t.Fatal(err)
	}
	f.jobs, err = media.NewJobs(media.JobsConfig{Store: f.store, Registry: f.registry, Locker: media.PGLocker(env.Pool()), Journal: journal, Processes: queue, Pool: env.Pool(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	f.manifests = f.jobs.Manifests()
	uploads, err := media.NewUploads(media.UploadOptions{Store: f.store, Manifests: f.manifests})
	if err != nil {
		t.Fatal(err)
	}
	f.uploads = uploads
	if err = f.versions.Bind(versions.Media{Manifests: f.manifests, Uploads: uploads, Jobs: f.jobs}); err != nil {
		t.Fatal(err)
	}
	if err = f.catalog.BindMedia(catalogpkg.Media{Registry: f.registry, Versions: f.versions}); err != nil {
		t.Fatal(err)
	}
	f.host = pgtest.EmptySchema(t, t.Context(), env.Pool())
	if err = riverhelpers.ApplyMigrations(t.Context(), env.Pool(), f.host); err != nil {
		t.Fatal(err)
	}
	if _, err = riverhelpers.New(t.Context(), env.Pool(), &river.Config{Schema: f.host}, f.jobs.RiverJobs(), f.versions.RiverJobs()); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *purgeFixture) table(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }
func (f *purgeFixture) advance(d time.Duration)  { f.clock.Add(int64(d)) }
func (f *purgeFixture) create(count int) []versions.Target {
	f.t.Helper()
	input := catalogpkg.ContentInput{Kind: "gallery", Text: map[string]catalogpkg.Text{"en": {Title: "Purge fixture"}}}
	for range count {
		input.Versions = append(input.Versions, catalogpkg.VersionInput{Languages: []string{"en"}})
	}
	ids, err := f.catalog.Create(f.t.Context(), f.actor, []catalogpkg.ContentInput{input})
	if err != nil {
		f.t.Fatal(err)
	}
	rows, err := f.env.Pool().Query(f.t.Context(), `SELECT content_id,id FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND content_id=$2 ORDER BY position,id`, f.env.Tenant, ids[0])
	if err != nil {
		f.t.Fatal(err)
	}
	targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (versions.Target, error) {
		var target versions.Target
		err := row.Scan(&target.Content, &target.Version)
		return target, err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return targets
}
func (f *purgeFixture) page(target versions.Target) string {
	f.t.Helper()
	ref, err := f.registry.VersionRef("gallery", target.Content, target.Version)
	if err != nil {
		f.t.Fatal(err)
	}
	body := pngOf(f.t, 16, 16, 1)
	sum := sha256.Sum256(body)
	blobs, err := f.manifests.NewBlobs(f.t.Context(), ref, layout.AreaPrivate, [][]byte{sum[:]})
	if err != nil {
		f.t.Fatal(err)
	}
	item, _ := f.registry.Item(ref)
	key, _ := item.Blob(blobs[0])
	if _, err = f.store.Put(f.t.Context(), key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/png"}); err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.manifests.Edit(f.t.Context(), ref, func(m *media.Manifest) error {
		m.Files = []media.File{{Path: "pages/01.png", Type: "image/png", Blob: blobs[0]}}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return key
}
func (f *purgeFixture) remove(targets ...versions.Target) {
	f.t.Helper()
	keys := make([]catalogpkg.VersionKey, len(targets))
	for i, v := range targets {
		keys[i] = catalogpkg.VersionKey{ContentID: v.Content, VersionID: v.Version}
	}
	if err := f.catalog.DeleteVersions(f.t.Context(), f.actor, keys); err != nil {
		f.t.Fatal(err)
	}
}
func (f *purgeFixture) exists(target versions.Target) bool {
	f.t.Helper()
	var exists bool
	if err := f.env.Pool().QueryRow(f.t.Context(), `SELECT EXISTS(SELECT 1 FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=$2)`, f.env.Tenant, target.Version).Scan(&exists); err != nil {
		f.t.Fatal(err)
	}
	return exists
}
func (f *purgeFixture) assertStorage(key string, exists bool) {
	f.t.Helper()
	_, err := f.env.Store.Head(f.t.Context(), key)
	if exists && err != nil || !exists && !errors.Is(err, media.ErrNotFound) {
		f.t.Fatalf("storage %s exists=%v: %v", key, exists, err)
	}
}
func (f *purgeFixture) purge() (int, error) {
	ctx, cancel := context.WithTimeout(f.t.Context(), 15*time.Second)
	defer cancel()
	return f.catalog.PurgeDeleted(ctx)
}
func (f *purgeFixture) assertNoFolderDeletionJob() {
	f.t.Helper()
	var n int
	if err := f.env.Pool().QueryRow(f.t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.host, "river_job"}.Sanitize()+` WHERE kind='contentkit_media_delete_folder'`).Scan(&n); err != nil || n != 0 {
		f.t.Fatalf("purge delegated %d folder jobs: %v", n, err)
	}
}

type purgeResult struct {
	count int
	err   error
}

// Stop after the catalog claim has committed, before a storage fence can land.
func (f *purgeFixture) blockedPurge() (func(), <-chan purgeResult) {
	f.t.Helper()
	entered, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	f.store.beforeFence = func() { close(entered); <-released }
	done := make(chan purgeResult, 1)
	go func() { n, err := f.purge(); done <- purgeResult{n, err}; close(done) }()
	f.t.Cleanup(func() {
		release()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			f.t.Error("purge did not stop")
		}
	})
	select {
	case <-entered:
	case result := <-done:
		f.t.Fatalf("purge did not reach fence: %v", result.err)
	case <-time.After(10 * time.Second):
		f.t.Fatal("purge did not reach storage after claiming")
	}
	return release, done
}

func TestCatalogPurgeKeepsRowsOnStorageFailureAndRetriesExactVersions(t *testing.T) {
	f := newPurgeFixture(t)
	targets := f.create(2)
	gone, kept := targets[0], targets[1]
	goneKey, keptKey := f.page(gone), f.page(kept)
	if err := f.versions.Delete(t.Context(), []versions.Target{gone}); err == nil {
		t.Fatal("direct storage deletion skipped its required catalog claim")
	}
	f.assertStorage(goneKey, true)
	f.remove(gone)
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("retention ignored: %d %v", n, err)
	}
	f.assertStorage(goneKey, true)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	f.store.failDelete.Store(true)
	if _, err := f.purge(); err == nil {
		t.Fatal("storage failure reported successful purge")
	}
	if !f.exists(gone) {
		t.Fatal("storage failure discarded catalog row")
	}
	f.assertStorage(goneKey, true)
	f.assertStorage(keptKey, true)
	f.store.failDelete.Store(false)
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
	if f.exists(gone) || !f.exists(kept) {
		t.Fatal("retry purged wrong versions")
	}
	f.assertStorage(goneKey, false)
	f.assertStorage(keptKey, true)
	f.assertNoFolderDeletionJob()
}

func TestCatalogPurgeClaimBlocksRestoreBeforeStorageFence(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(map[bool]string{false: "version", true: "last-version"}[whole], func(t *testing.T) {
			f := newPurgeFixture(t)
			count := 2
			if whole {
				count = 1
			}
			target := f.create(count)[0]
			key := f.page(target)
			f.remove(target)
			f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
			release, done := f.blockedPurge()
			f.assertStorage(key, true)
			_, err := f.catalog.PutVersions(t.Context(), f.actor, []catalogpkg.VersionPut{{ContentID: target.Content, VersionID: target.Version, VersionInput: catalogpkg.VersionInput{State: catalogpkg.StateDraft}}})
			if !errors.Is(err, catalogpkg.ErrConflict) {
				t.Fatalf("restore escaped committed erasure claim before S3 fence: %v", err)
			}
			release()
			if result := <-done; result.err != nil {
				t.Fatal(result.err)
			}
			if f.exists(target) {
				t.Fatal("claimed version survived completed purge")
			}
			f.assertStorage(key, false)
		})
	}
}

func TestCatalogRestoreBeforePurgeKeepsStorage(t *testing.T) {
	f := newPurgeFixture(t)
	target := f.create(1)[0]
	key := f.page(target)
	f.remove(target)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	if _, err := f.catalog.PutVersions(t.Context(), f.actor, []catalogpkg.VersionPut{{ContentID: target.Content, VersionID: target.Version, VersionInput: catalogpkg.VersionInput{State: catalogpkg.StateDraft}}}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("restored version was purged: %d %v", n, err)
	}
	if !f.exists(target) {
		t.Fatal("restored version disappeared")
	}
	f.assertStorage(key, true)
	f.assertNoFolderDeletionJob()
}

func TestCatalogPurgeOwnsLateWritesAfterRowsAreGone(t *testing.T) {
	f := newPurgeFixture(t)
	target := f.create(1)[0]
	key := f.page(target)
	f.remove(target)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("initial purge: %d %v", n, err)
	}
	if f.exists(target) {
		t.Fatal("purged row remains")
	}
	// Simulate completion of a storage upload whose URL predates erasure.
	if _, err := f.env.Store.Put(t.Context(), key, strings.NewReader("late"), 4, media.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	f.advance(8 * 24 * time.Hour)
	if _, err := f.jobs.CleanupDue(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	f.assertStorage(key, true)
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("late-write retry: %d %v", n, err)
	}
	f.assertStorage(key, false)
	f.assertNoFolderDeletionJob()
}

func TestCatalogPurgeRechecksOnlyItsClaimedIDs(t *testing.T) {
	f := newPurgeFixture(t)
	first := f.create(1)[0]
	firstKey := f.page(first)
	f.remove(first)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	release, done := f.blockedPurge()
	// Another deletion becomes eligible while the first batch is in storage I/O.
	second := f.create(1)[0]
	secondKey := f.page(second)
	f.remove(second)
	old := time.Unix(0, f.clock.Load()).Add(-catalogpkg.DefaultDeletedRetention - time.Hour)
	for _, table := range []string{"content_items", "content_versions"} {
		id := second.Content
		if table == "content_versions" {
			id = second.Version
		}
		if _, err := f.env.Pool().Exec(t.Context(), `UPDATE `+f.table(table)+` SET deleted_at=$3 WHERE tenant_id=$1 AND id=$2`, f.env.Tenant, id, old); err != nil {
			t.Fatal(err)
		}
	}
	release()
	if result := <-done; result.err != nil || result.count != 1 {
		t.Fatalf("claimed batch: %+v", result)
	}
	if f.exists(first) || !f.exists(second) {
		t.Fatal("final SQL phase substituted a newly eligible item")
	}
	f.assertStorage(firstKey, false)
	f.assertStorage(secondKey, true)
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("next batch: %d %v", n, err)
	}
	f.assertStorage(secondKey, false)
}

func TestCatalogPurgeSkipsConcurrentRestorationLocks(t *testing.T) {
	f := newPurgeFixture(t)
	target := f.create(1)[0]
	key := f.page(target)
	f.remove(target)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	tx, err := f.env.Pool().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	// Hold the same parent/version locks as an authorized restoration. Purge
	// must leave this batch alone until the restoring transaction commits.
	for _, entry := range []struct{ table, id string }{{"content_items", target.Content}, {"content_versions", target.Version}} {
		if _, err = tx.Exec(t.Context(), `SELECT FROM `+f.table(entry.table)+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, f.env.Tenant, entry.id); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("purge crossed restoration locks: %d %v", n, err)
	}
	for _, entry := range []struct{ table, id string }{{"content_items", target.Content}, {"content_versions", target.Version}} {
		if _, err = tx.Exec(t.Context(), `UPDATE `+f.table(entry.table)+` SET state='draft',deleted_at=NULL WHERE tenant_id=$1 AND id=$2`, f.env.Tenant, entry.id); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("restored batch: %d %v", n, err)
	}
	if !f.exists(target) {
		t.Fatal("restored row disappeared")
	}
	f.assertStorage(key, true)
}

func TestCatalogPurgeRecoversAmbiguousFenceResponse(t *testing.T) {
	f := newPurgeFixture(t)
	target := f.create(1)[0]
	key := f.page(target)
	f.remove(target)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	f.store.ambiguousFence.Store(true)
	if _, err := f.purge(); err == nil {
		t.Fatal("lost fence response reported complete")
	}
	if !f.exists(target) {
		t.Fatal("ambiguous storage result discarded the catalog record")
	}
	_, err := f.catalog.PutVersions(t.Context(), f.actor, []catalogpkg.VersionPut{{ContentID: target.Content, VersionID: target.Version, VersionInput: catalogpkg.VersionInput{State: catalogpkg.StateDraft}}})
	if !errors.Is(err, catalogpkg.ErrConflict) {
		t.Fatalf("ambiguous erasure allowed restoration: %v", err)
	}
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("fence retry: %d %v", n, err)
	}
	f.assertStorage(key, false)
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("completed retry repeated work: %d %v", n, err)
	}
	f.assertNoFolderDeletionJob()
}

func TestCatalogPurgeLateFailureDoesNotStarveOtherExpiredItems(t *testing.T) {
	f := newPurgeFixture(t)
	old := f.create(1)[0]
	oldKey := f.page(old)
	f.remove(old)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("first purge: %d %v", n, err)
	}
	current := f.create(1)[0]
	currentKey := f.page(current)
	f.remove(current)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	if _, err := f.env.Store.Put(t.Context(), oldKey, strings.NewReader("late"), 4, media.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	f.store.failPrefix = oldKey
	f.store.failDelete.Store(true)
	if n, err := f.purge(); err == nil || n != 1 {
		t.Fatalf("late failure blocked unrelated eligible content or hid error: %d %v", n, err)
	}
	if f.exists(current) {
		t.Fatal("unrelated eligible content was starved")
	}
	f.assertStorage(currentKey, false)
	f.assertStorage(oldKey, true)
	f.store.failDelete.Store(false)
	if n, err := f.purge(); err != nil || n != 0 {
		t.Fatalf("late failure recovery: %d %v", n, err)
	}
	f.assertStorage(oldKey, false)
}

func (f *purgeFixture) recentDeletion(target versions.Target) {
	f.t.Helper()
	recent := time.Unix(0, f.clock.Load()).Add(-24 * time.Hour)
	for _, entry := range []struct{ table, id string }{{"content_items", target.Content}, {"content_versions", target.Version}} {
		if _, err := f.env.Pool().Exec(f.t.Context(), `UPDATE `+f.table(entry.table)+` SET deleted_at=$3 WHERE tenant_id=$1 AND id=$2`, f.env.Tenant, entry.id, recent); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestCatalogPurgeClaimDoesNotBlockRetainedSiblingRestore(t *testing.T) {
	f := newPurgeFixture(t)
	targets := f.create(2)
	old, retained := targets[0], targets[1]
	oldKey, retainedKey := f.page(old), f.page(retained)
	f.remove(old)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	f.store.failPrefix = oldKey
	f.store.failDelete.Store(true)
	if _, err := f.purge(); err == nil {
		t.Fatal("expected old version's storage failure")
	}
	f.remove(retained)
	f.recentDeletion(retained)
	if _, err := f.catalog.PutVersions(t.Context(), f.actor, []catalogpkg.VersionPut{{ContentID: retained.Content, VersionID: retained.Version, VersionInput: catalogpkg.VersionInput{State: catalogpkg.StateDraft}}}); err != nil {
		t.Fatalf("retained sibling could not restore its automatically deleted parent: %v", err)
	}
	f.assertStorage(retainedKey, true)
	if _, err := f.catalog.PutVersions(t.Context(), f.actor, []catalogpkg.VersionPut{{ContentID: old.Content, VersionID: old.Version, VersionInput: catalogpkg.VersionInput{State: catalogpkg.StateDraft}}}); !errors.Is(err, catalogpkg.ErrConflict) {
		t.Fatalf("claimed old version became restorable: %v", err)
	}
	f.store.failDelete.Store(false)
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("old version retry: %d %v", n, err)
	}
	if !f.exists(retained) || f.exists(old) {
		t.Fatal("retry damaged the restored sibling")
	}
	f.assertStorage(oldKey, false)
	f.assertStorage(retainedKey, true)
}

func TestCatalogPurgeRetriesOldVersionUnderRecentlyDeletedParent(t *testing.T) {
	f := newPurgeFixture(t)
	targets := f.create(2)
	old, retained := targets[0], targets[1]
	oldKey, retainedKey := f.page(old), f.page(retained)
	f.remove(old)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	f.store.failPrefix = oldKey
	f.store.failDelete.Store(true)
	if _, err := f.purge(); err == nil {
		t.Fatal("expected old version's storage failure")
	}
	f.remove(retained)
	f.recentDeletion(retained)
	f.store.failDelete.Store(false)
	if n, err := f.purge(); err != nil || n != 1 {
		t.Fatalf("recent parent postponed expired version retry: %d %v", n, err)
	}
	if f.exists(old) || !f.exists(retained) {
		t.Fatal("individual retention deadlines were not preserved")
	}
	f.assertStorage(oldKey, false)
	f.assertStorage(retainedKey, true)
}

func TestCatalogPurgeInitialFailureDoesNotStarveOtherTargets(t *testing.T) {
	for _, mode := range []string{"parents", "siblings", "cross-phase"} {
		t.Run(mode, func(t *testing.T) {
			f := newPurgeFixture(t)
			var bad, good versions.Target
			switch mode {
			case "parents":
				bad = f.create(1)[0]
				good = f.create(1)[0]
			case "siblings":
				targets := f.create(3)
				bad, good = targets[0], targets[1]
			case "cross-phase":
				bad = f.create(1)[0]
				good = f.create(2)[0]
			}
			badKey, goodKey := f.page(bad), f.page(good)
			f.remove(bad, good)
			f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
			f.store.failPrefix = badKey
			f.store.failDelete.Store(true)
			if n, err := f.purge(); err == nil || n != 1 {
				t.Fatalf("healthy target was starved or failure was hidden: count=%d err=%v", n, err)
			}
			if !f.exists(bad) || f.exists(good) {
				t.Fatal("failed and successful catalog records were not isolated")
			}
			f.assertStorage(badKey, true)
			f.assertStorage(goodKey, false)
			f.store.failDelete.Store(false)
			if n, err := f.purge(); err != nil || n != 1 {
				t.Fatalf("failed target retry: count=%d err=%v", n, err)
			}
			f.assertStorage(badKey, false)
			f.assertNoFolderDeletionJob()
		})
	}
}

func TestCatalogPurgeDoesNotDeleteNewVersionRevivingItsParent(t *testing.T) {
	f := newPurgeFixture(t)
	old := f.create(1)[0]
	oldKey := f.page(old)
	f.remove(old)
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	release, done := f.blockedPurge()
	ids, err := f.catalog.PutVersions(t.Context(), f.actor, []catalogpkg.VersionPut{{ContentID: old.Content, VersionInput: catalogpkg.VersionInput{Languages: []string{"en"}}}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("new version could not revive its parent independently: %v", err)
	}
	fresh := versions.Target{Content: old.Content, Version: ids[0]}
	freshKey := f.page(fresh)
	release()
	if result := <-done; result.err != nil || result.count != 1 {
		t.Fatalf("purge after new sibling: %+v", result)
	}
	if !f.exists(fresh) || f.exists(old) {
		t.Fatal("captured parent purge removed the newly created version")
	}
	f.assertStorage(oldKey, false)
	f.assertStorage(freshKey, true)
}

func TestCatalogPurgeStillBlocksExplicitParentRestoreOfClaimedVersions(t *testing.T) {
	f := newPurgeFixture(t)
	target := f.create(1)[0]
	key := f.page(target)
	if err := f.catalog.DeleteForModeration(t.Context(), f.actor, []string{target.Content}); err != nil {
		t.Fatal(err)
	}
	f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
	release, done := f.blockedPurge()
	if err := f.catalog.Restore(t.Context(), f.actor, []string{target.Content}); !errors.Is(err, catalogpkg.ErrConflict) {
		t.Fatalf("parent restoration exposed its claimed version: %v", err)
	}
	release()
	if result := <-done; result.err != nil || result.count != 1 {
		t.Fatalf("parent purge: %+v", result)
	}
	f.assertStorage(key, false)
}

func TestUploadOperationCannotReserveAnotherVersionsPurgeIDs(t *testing.T) {
	for _, operation := range []string{"claim", "erase"} {
		t.Run(operation, func(t *testing.T) {
			f := newPurgeFixture(t)
			own, victim := f.create(1)[0], f.create(1)[0]
			victimKey := f.page(victim)
			attacker := testauth.Actor(access.Actor{ID: contentref.NewID()}, &grants{})
			if _, err := f.env.Pool().Exec(t.Context(), `UPDATE `+f.table("content_creators")+` SET user_id=$3 WHERE tenant_id=$1 AND content_id=$2`, f.env.Tenant, own.Content, attacker.ID); err != nil {
				t.Fatal(err)
			}
			ownRef, _ := f.registry.VersionRef("gallery", own.Content, own.Version)
			victimRef, _ := f.registry.VersionRef("gallery", victim.Content, victim.Version)
			ops := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "owned upload"}}}
			if _, err := f.uploads.Commit(t.Context(), attacker, ownRef, uuid.NewString(), ops); err != nil {
				t.Fatalf("uploader cannot edit its own item: %v", err)
			}
			if _, err := f.uploads.Commit(t.Context(), attacker, victimRef, uuid.NewString(), ops); err == nil {
				t.Fatal("fixture uploader unexpectedly controls the victim")
			}
			item, _ := f.registry.Item(victimRef)
			name := "erasure-claim:" + item.Prefix()
			if operation == "erase" {
				name = "erase:" + item.Prefix() + "content-purge:" + victim.Version
			}
			reserved := uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
			_, err := f.uploads.Commit(t.Context(), attacker, ownRef, reserved, ops)
			invalid, ok := media.AsUploadError(err)
			if !ok || invalid.Code != media.CodeInvalid {
				t.Errorf("client reserved an internal purge UUID: %v", err)
			}
			var receipts int
			if err := f.env.Pool().QueryRow(t.Context(), `SELECT count(*) FROM `+f.table("content_media_commits")+` WHERE tenant_id=$1 AND operation_id=$2::uuid`, f.env.Tenant, reserved).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if receipts != 0 {
				t.Errorf("rejected client ID inserted %d journal receipts", receipts)
			}
			f.remove(victim)
			f.advance(catalogpkg.DefaultDeletedRetention + time.Hour)
			if n, err := f.purge(); err != nil || n != 1 {
				t.Errorf("another item's client ID blocked victim purge: count=%d error=%v identity_collision=%v", n, err, errors.Is(err, media.ErrCommitIdentity))
				return
			}
			if f.exists(victim) || !f.exists(own) {
				t.Fatal("purge affected the wrong item")
			}
			f.assertStorage(victimKey, false)
		})
	}
}

func TestUploadClientOperationUUIDVersionsPreserveExactReplay(t *testing.T) {
	for _, version := range []int{4, 7} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			f := newPurgeFixture(t)
			target := f.create(1)[0]
			ref, _ := f.registry.VersionRef("gallery", target.Content, target.Version)
			id := uuid.New()
			if version == 7 {
				id = uuid.Must(uuid.NewV7())
			}
			original := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "first"}}}
			later := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "later"}}}
			if _, err := f.uploads.Commit(t.Context(), f.actor, ref, id.String(), original); err != nil {
				t.Fatal(err)
			}
			if _, err := f.uploads.Commit(t.Context(), f.actor, ref, uuid.NewString(), later); err != nil {
				t.Fatal(err)
			}
			_, before, err := f.manifests.Get(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := f.uploads.CommitChecked(t.Context(), f.actor, ref, id.String(), original, func(*media.Manifest) error { return errors.New("obsolete precondition must not run on exact replay") })
			if err != nil || replayed.Meta["title"] != "later" {
				t.Fatalf("exact replay repeated or rejected the edit: %+v %v", replayed, err)
			}
			_, after, err := f.manifests.Get(t.Context(), ref)
			if err != nil || before != after {
				t.Fatalf("exact replay changed storage: %v", err)
			}
			_, err = f.uploads.Commit(t.Context(), f.actor, ref, id.String(), later)
			conflict, ok := media.AsUploadError(err)
			if !ok || conflict.Code != media.CodeConflict {
				t.Fatalf("same ID accepted different inputs: %v", err)
			}
		})
	}
}

func TestUploadDeterministicOperationsRequireExactTrustedSystemActor(t *testing.T) {
	f := newPurgeFixture(t)
	target := f.create(1)[0]
	ref, _ := f.registry.VersionRef("gallery", target.Content, target.Version)
	ops := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "internal"}}}
	for i, actor := range []access.Actor{{ID: "contentkit", Kind: "user"}, {ID: "another", Kind: "contentkit:system"}, {ID: "contentkit", Kind: "contentkit:system", Anonymous: true}} {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("untrusted-system-match-"+string(rune('0'+i))))
		_, err := f.uploads.Commit(t.Context(), actor, ref, id.String(), ops)
		invalid, ok := media.AsUploadError(err)
		if !ok || invalid.Code != media.CodeInvalid {
			t.Fatalf("nonmatching actor reserved deterministic ID: %+v %v", actor, err)
		}
	}
	malformed := uuid.New()
	malformed[8] &= 0x3f
	_, err := f.uploads.Commit(t.Context(), f.actor, ref, malformed.String(), ops)
	invalid, ok := media.AsUploadError(err)
	if !ok || invalid.Code != media.CodeInvalid {
		t.Fatalf("non-RFC UUID variant was accepted: %v", err)
	}
	trusted := access.Actor{ID: "contentkit", Kind: "contentkit:system"}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("trusted-system-commit"))
	for range 2 {
		got, err := f.uploads.Commit(t.Context(), trusted, ref, id.String(), ops)
		if err != nil || got.Meta["title"] != "internal" {
			t.Fatalf("trusted canonical deterministic operation failed: %v", err)
		}
	}
}
