package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// A lost PUT response is not proof of failure, and reading the old object is
// not proof that a delayed PUT cannot still land. Recovery must fence both a
// missing root and an existing root before recording an absent outcome.
func TestManifestCommitRecoveryFencesLateWrites(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t/applied=%t", existing, applied), func(t *testing.T) {
				env := s3test.Open(t)
				pool := pgtest.Pool(t, nil)
				schema := pgtest.Schema(t, t.Context(), pool)
				journal, err := media.NewPGJournal(pool, schema, nil)
				if err != nil {
					t.Fatal(err)
				}
				reg := miniRegistry(t, env.Tenant)
				ref := contentref.New(env.Tenant, "post", cid(7))
				item, _ := reg.Item(ref)
				store := &uncertainManifestStore{Store: env.Store, key: item.ManifestKey(), applied: applied}
				ms, err := media.NewManifests(store, reg, media.ManifestOptions{Locker: media.PGLocker(pool), Journal: journal})
				if err != nil {
					t.Fatal(err)
				}
				if existing {
					if _, err := ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
						m.Meta = map[string]any{"title": "before"}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				store.uncertain = true
				if _, err := ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
					m.Meta = map[string]any{"title": "attempt"}
					return nil
				}); !errors.Is(err, media.ErrUnavailable) {
					t.Fatalf("lost response: %v", err)
				}
				table := pgx.Identifier{schema, "content_media_commits"}.Sanitize()
				// An older pending item belongs to a different registry using
				// this namespace/schema. It must not occupy our discovery limit.
				if _, err := pool.Exec(t.Context(), `INSERT INTO `+table+`
(tenant_id, operation_id, content_kind, content_id, folder_prefix, actor_id, fingerprint, lease_id, updated_at)
VALUES ($1, gen_random_uuid(), 'other-kind', $2, $3, '', decode(repeat('00', 32), 'hex'), gen_random_uuid(), now() - interval '1 day')`,
					ref.TenantID, cid(8), ref.TenantID+"/other-kind/"+cid(8)+"/"); err != nil {
					t.Fatal(err)
				}
				state := func(want string) {
					t.Helper()
					var got string
					if err := pool.QueryRow(t.Context(), `SELECT state FROM `+table+`
WHERE tenant_id = $1 AND folder_prefix = $2 ORDER BY created_at DESC LIMIT 1`, ref.TenantID, item.Prefix()).Scan(&got); err != nil || got != want {
						t.Fatalf("journal state %q, want %q: %v", got, want, err)
					}
				}
				state("prepared")
				store.blockFence = true
				if err := ms.RecoverPending(t.Context(), 1); !errors.Is(err, media.ErrUnavailable) {
					t.Fatalf("unreachable fence: %v", err)
				}
				state("frozen")
				store.blockFence = false
				if err := ms.RecoverPending(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				wantState, wantTitle := "absent", ""
				if applied {
					wantState, wantTitle = "applied", "attempt"
				} else if existing {
					wantTitle = "before"
				}
				state(wantState)
				if _, err := env.Store.Put(t.Context(), item.ManifestKey(), bytes.NewReader(store.body), int64(len(store.body)), store.options); !errors.Is(err, media.ErrPreconditionFailed) {
					t.Fatalf("delayed original PUT after recovery: %v", err)
				}
				man, etag, err := ms.Get(t.Context(), ref)
				if err != nil {
					t.Fatal(err)
				}
				if title, _ := man.Meta["title"].(string); title != wantTitle {
					t.Fatalf("recovered manifest %+v, want title %q: %v", man, wantTitle, err)
				}
				if err := ms.RecoverPending(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				_, after, err := ms.Get(t.Context(), ref)
				if err != nil || etag != after {
					t.Fatalf("settled recovery was repeated: %q/%q %v", etag, after, err)
				}
				if _, err := ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
					m.Meta = map[string]any{"title": "later"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLateCommitCannotAcknowledgeAnotherOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	env := s3test.Open(t)
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	journal, err := media.NewPGJournal(pool, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := miniRegistry(t, env.Tenant)
	ref := contentref.New(env.Tenant, "post", cid(7))
	item, _ := reg.Item(ref)
	prepared, release := make(chan struct{}), make(chan struct{})
	var delayed atomic.Bool
	store := &manifestPutStore{Store: env.Store, put: func(ctx context.Context, key string, body io.Reader, size int64, opts media.PutOptions) (media.Object, error) {
		if key == item.ManifestKey() && delayed.CompareAndSwap(false, true) {
			close(prepared)
			select {
			case <-release:
			case <-ctx.Done():
				return media.Object{}, ctx.Err()
			}
		}
		return env.Store.Put(ctx, key, body, size, opts)
	}}
	// No lock models A losing its advisory session while its network request
	// remains alive; B may now recover and edit the same item.
	a, err := media.NewManifests(store, reg, media.ManifestOptions{Locker: noLock{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	bStore := &uncertainManifestStore{Store: env.Store, key: item.ManifestKey()}
	b, err := media.NewManifests(bStore, reg, media.ManifestOptions{Locker: noLock{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	aDone := make(chan error, 1)
	go func() {
		_, err := a.Edit(ctx, ref, func(m *media.Manifest) error {
			m.Meta = map[string]any{"title": "A"}
			return nil
		})
		aDone <- err
	}()
	select {
	case <-prepared:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// B first fences A, then loses its own response. Keep B prepared until A
	// receives its late 412; A must not settle or acknowledge B's receipt.
	if err := b.Recover(ctx, ref); err != nil {
		t.Fatal(err)
	}
	bStore.uncertain, bStore.applied = true, true
	if _, err := b.Edit(ctx, ref, func(m *media.Manifest) error {
		m.Meta = map[string]any{"title": "B"}
		return nil
	}); !errors.Is(err, media.ErrUnavailable) {
		t.Fatal(err)
	}
	close(release)
	if err := <-aDone; !errors.Is(err, media.ErrManifestConflict) {
		t.Fatalf("A acknowledged another operation or restarted: %v", err)
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "content_media_commits"}.Sanitize()+`
WHERE tenant_id = $1 AND state = 'prepared'`, ref.TenantID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("A consumed B's pending receipt: %d %v", pending, err)
	}
	if err := b.Recover(ctx, ref); err != nil {
		t.Fatal(err)
	}
	man, _, err := b.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if man.Meta["title"] != "B" {
		t.Fatalf("A overwrote B: %+v", man)
	}
}

type manifestPutStore struct {
	media.Store
	put func(context.Context, string, io.Reader, int64, media.PutOptions) (media.Object, error)
}

// Quota follows the fenced S3 outcome, not the caller's observed PUT error.
func TestUploadQuotaRecoversWithManifestOutcome(t *testing.T) {
	for _, action := range []string{"insert", "replace", "rename", "remove"} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/applied=%t", action, applied), func(t *testing.T) {
				f := newFixture(t)
				f.visible(1)
				ref := f.ref("gallery", 1)
				item, _ := f.reg.Item(ref)
				store := &uncertainManifestStore{Store: f.env.Store, key: item.ManifestKey(), applied: applied}
				manifests := s3test.Manifests(t, store, f.reg, media.ManifestOptions{Journal: f.journal})
				quota := int64(1 << 20)
				var quotaErr error
				limiter, err := media.NewPGLimiter(f.env.Pool(), f.env.ContentSchema(), media.PGLimits{
					Quota: func(context.Context, string, string) (int64, error) { return quota, quotaErr },
				})
				if err != nil {
					t.Fatal(err)
				}
				f.up, err = media.NewUploads(media.UploadOptions{Store: store, Manifests: manifests, Limiter: limiter})
				if err != nil {
					t.Fatal(err)
				}
				var before int64
				if action != "insert" {
					before = f.put(ref, "originals/one.png", "image/png", []byte("old")).UploadBytes()
				}
				f.q.take()
				var ops []media.Op
				var delta, reserved int64
				switch action {
				case "insert", "replace":
					body := bytes.Repeat([]byte("n"), 96<<10) // above the existing per-upload accounting floor
					p, staged := f.upload(ref, "originals/one.png", "image/png", body)
					ops = []media.Op{{Op: media.OpPut, Path: p, Blob: staged}}
					reserved, delta = int64(len(body)), int64(len(body))-before
				case "rename":
					ops = []media.Op{{Op: media.OpRename, Path: "originals/one.png", To: "originals/two.png"}}
				case "remove":
					ops = []media.Op{{Op: media.OpRemove, Path: "originals/one.png"}}
					delta = -before
				}
				usage := func(wantUsed, wantPending int64) {
					t.Helper()
					used, pending, err := limiter.Usage(t.Context(), ref.TenantID, "owner")
					if err != nil || used != wantUsed || pending != wantPending {
						t.Fatalf("usage=%d pending=%d, want %d/%d: %v", used, pending, wantUsed, wantPending, err)
					}
				}
				if delta > 0 {
					_, beforeETag, beforeErr := manifests.Get(t.Context(), ref)
					if beforeErr != nil && !errors.Is(beforeErr, media.ErrNotFound) {
						t.Fatal(beforeErr)
					}
					quota = before + delta - 1 // the cap changed after the upload was reserved
					if _, err := f.up.Commit(t.Context(), f.editor, ref, uuid.NewString(), ops); code(err) != media.CodeQuota {
						t.Fatalf("growth past current quota was accepted: %v", err)
					}
					usage(before, reserved)
					_, afterETag, afterErr := manifests.Get(t.Context(), ref)
					if beforeETag != afterETag || errors.Is(beforeErr, media.ErrNotFound) != errors.Is(afterErr, media.ErrNotFound) || afterErr != nil && !errors.Is(afterErr, media.ErrNotFound) {
						t.Fatalf("quota refusal changed S3: %q/%q %v/%v", beforeETag, afterETag, beforeErr, afterErr)
					}
					quota = 1 << 20
				} else {
					quotaErr = errors.New("quota policy unavailable")
				}
				store.uncertain = true
				operationID := uuid.NewString()
				if _, err := f.up.Commit(t.Context(), f.editor, ref, operationID, ops); !errors.Is(err, media.ErrUnavailable) {
					t.Fatalf("uncertain write: %v", err)
				}
				usage(before+max(delta, 0), reserved)
				if jobs := f.q.take(); len(jobs) != 0 {
					t.Fatalf("uncertain commit queued processing before recovery: %+v", jobs)
				}
				store.blockFence = true
				if err := manifests.Recover(t.Context(), ref); !errors.Is(err, media.ErrUnavailable) {
					t.Fatalf("failed fence: %v", err)
				}
				usage(before+max(delta, 0), reserved)
				store.blockFence = false
				for range 2 {
					if err := manifests.Recover(t.Context(), ref); err != nil {
						t.Fatal(err)
					}
					if applied {
						usage(before+delta, 0)
					} else {
						usage(before, reserved)
					}
				}
				wantJobs := 0
				if applied {
					wantJobs = 1
				}
				if jobs := f.q.take(); len(jobs) != wantJobs {
					t.Fatalf("recovery queued %d jobs, want %d: %+v", len(jobs), wantJobs, jobs)
				}
				if _, err := f.env.Store.Put(t.Context(), item.ManifestKey(), bytes.NewReader(store.body), int64(len(store.body)), store.options); !errors.Is(err, media.ErrPreconditionFailed) {
					t.Fatalf("late original write was not fenced: %v", err)
				}
				// Reuse the original identity, including the absent branch. A
				// successful replay must not charge or queue the batch twice.
				for range 2 {
					if _, err := f.up.Commit(t.Context(), f.editor, ref, operationID, ops); err != nil {
						t.Fatalf("same-batch retry: %v", err)
					}
					usage(before+delta, 0)
				}
				wantJobs = 0
				if !applied {
					wantJobs = 1
				}
				if jobs := f.q.take(); len(jobs) != wantJobs {
					t.Fatalf("batch retry queued %d jobs, want %d", len(jobs), wantJobs)
				}
			})
		}
	}
}

func TestForeignJournalCannotEditOrCleanReceipt(t *testing.T) {
	env := s3test.Open(t)
	pool := pgtest.Pool(t, nil)
	reg := miniRegistry(t, env.Tenant)
	ref, _ := reg.Ref("post", cid(7))
	item, _ := reg.Item(ref)
	store := &uncertainManifestStore{Store: env.Store, key: item.ManifestKey(), uncertain: true, applied: true}
	owner, err := media.NewManifests(store, reg, media.ManifestOptions{Locker: media.PGLocker(pool), Journal: env.Journal()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Edit(t.Context(), ref, func(m *media.Manifest) error {
		m.Meta = map[string]any{"title": "owner"}
		return nil
	}); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("lost response: %v", err)
	}
	foreign, err := media.NewPGJournal(pool, pgtest.Schema(t, t.Context(), pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: reg, Locker: media.PGLocker(pool), Journal: foreign,
		Now: func() time.Time { return time.Now().Add(72 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	blob := blobOf([]byte("stray"))
	private, _ := item.Blob(blob)
	public, _ := item.Public("stray.webp")
	for _, key := range []string{private, public} {
		if _, err := env.Store.Put(t.Context(), key, strings.NewReader("stray"), 5, media.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, settled := range []bool{false, true} {
		if settled {
			if err := owner.Recover(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
		}
		_, etag, err := owner.Get(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		for name, action := range map[string]func() error{
			"edit": func() error {
				_, err := jobs.Manifests().Edit(t.Context(), ref, func(*media.Manifest) error {
					t.Error("foreign journal ran an edit callback")
					return nil
				})
				return err
			},
			"public cleanup": func() error { _, err := jobs.Manifests().SyncPublic(t.Context(), ref); return err },
			"private cleanup": func() error {
				return jobs.Manifests().DropUnreferenced(t.Context(), ref, media.Unreferenced{Blobs: []string{blob}})
			},
			"sweep": func() error { _, err := jobs.Sweep(t.Context(), ref); return err },
		} {
			if err := action(); !errors.Is(err, media.ErrCommitPending) {
				t.Fatalf("%s (settled=%t) accepted a foreign receipt: %v", name, settled, err)
			}
		}
		if _, after, err := owner.Get(t.Context(), ref); err != nil || after != etag {
			t.Fatalf("foreign journal changed the manifest revision: %q/%q %v", etag, after, err)
		}
		for _, key := range []string{private, public} {
			if _, err := env.Store.Head(t.Context(), key); err != nil {
				t.Fatalf("foreign journal deleted %s: %v", key, err)
			}
		}
	}
}

func (s *manifestPutStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts media.PutOptions) (media.Object, error) {
	return s.put(ctx, key, body, size, opts)
}

type uncertainManifestStore struct {
	media.Store
	key        string
	uncertain  bool
	applied    bool
	blockFence bool
	body       []byte
	options    media.PutOptions
}

func (s *uncertainManifestStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts media.PutOptions) (media.Object, error) {
	if key != s.key {
		return s.Store.Put(ctx, key, body, size, opts)
	}
	if s.blockFence {
		return media.Object{}, media.ErrUnavailable
	}
	if !s.uncertain {
		return s.Store.Put(ctx, key, body, size, opts)
	}
	s.uncertain = false
	data, err := io.ReadAll(body)
	if err != nil {
		return media.Object{}, err
	}
	s.body, s.options = data, opts
	if s.applied {
		if _, err := s.Store.Put(ctx, key, bytes.NewReader(data), size, opts); err != nil {
			return media.Object{}, err
		}
	}
	return media.Object{}, media.ErrUnavailable
}

// The lock orders cooperating writers; If-Match also protects against a
// writer outside that lock (for example a process whose session died).
func TestManifestsLockAndUseIfMatch(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	kinds := miniRegistry(t, env.Tenant)
	if _, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{}); err == nil {
		t.Fatal("lock-free Manifests must be refused")
	}
	locker := media.PGLocker(pgtest.Pool(t, nil))
	if _, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: locker}); err == nil {
		t.Fatal("journal-free Manifests must be refused")
	}
	probed, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: locker, Journal: env.Journal()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: locker, Journal: env.Journal()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := contentref.New(env.Tenant, "post", cid(7))
	set := func(m *media.Manifest, k string) {
		if m.Meta == nil {
			m.Meta = map[string]any{}
		}
		m.Meta[k] = true
	}
	var wg sync.WaitGroup
	for i := range 8 {
		ms := probed
		if i%2 == 1 {
			ms = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error { set(m, fmt.Sprint("k", i)); return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// A writer outside the lock lands between the probed edit's read and write.
	var once atomic.Bool
	if _, err := probed.Edit(ctx, ref, func(m *media.Manifest) error {
		if once.CompareAndSwap(false, true) {
			if _, err := outsideLock(t, env).Edit(ctx, ref, func(m *media.Manifest) error { set(m, "outside"); return nil }); err != nil {
				return err
			}
		}
		set(m, "inside")
		return nil
	}); !errors.Is(err, media.ErrCommitPending) && !errors.Is(err, media.ErrManifestConflict) {
		t.Fatalf("frozen caller was not refused: %v", err)
	}
	// A fresh request may proceed; the frozen caller must not silently start
	// another attempt against the replacement revision.
	if _, err := probed.Edit(ctx, ref, func(m *media.Manifest) error { set(m, "inside"); return nil }); err != nil {
		t.Fatal(err)
	}
	got, _, err := probed.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "outside", "inside"} {
		if got.Meta[k] != true {
			t.Fatalf("edit %s was lost: %v", k, got.Meta)
		}
	}
}

// outsideLock is a process with its own lock space (not the host's), so its
// edits do not wait on the caller's lock.
func outsideLock(t *testing.T, env *s3test.Env) *media.Manifests {
	ms, err := media.NewManifests(env.Store, miniRegistry(t, env.Tenant), media.ManifestOptions{Locker: noLock{}, Journal: env.Journal()})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

type noLock struct{}

func (noLock) Lock(context.Context, string) (func(), error) { return func() {}, nil }

func TestManifestsRefuseUnconditionalWrites(t *testing.T) {
	t.Parallel()
	env := s3test.Open(t)
	reg := miniRegistry(t, env.Tenant)
	locker := media.PGLocker(pgtest.Pool(t, nil))
	ms, err := media.NewManifests(env.WithCapabilities(t, media.Capabilities{}), reg, media.ManifestOptions{Locker: locker, Journal: env.Journal()})
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.New(env.Tenant, "post", cid(7))
	for _, tc := range []struct {
		name string
		edit func(context.Context, contentref.ContentRef, func(*media.Manifest) error) (*media.Manifest, error)
	}{
		{name: "create or edit", edit: ms.Edit},
		{name: "worker edit", edit: ms.EditExisting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.edit(t.Context(), ref, func(*media.Manifest) error {
				t.Error("callback ran without a storage fence")
				return nil
			})
			if !errors.Is(err, media.ErrConditionalPutRequired) {
				t.Fatalf("unqualified write: %v", err)
			}
		})
	}
	item, _ := reg.Item(ref)
	if _, err := env.Store.Head(t.Context(), item.ManifestKey()); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("unqualified create wrote a manifest: %v", err)
	}
}

func TestUploadsRefuseUnconditionalStorageBeforeMovingBytes(t *testing.T) {
	t.Parallel()
	env := s3test.Open(t).WithoutConditionalPut(t)
	f := newFixtureOn(t, env, nil)
	limiter, err := media.NewPGLimiter(env.Pool(), env.ContentSchema(), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	up, err := media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: f.ms, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	ref := f.ref("gallery", 1)
	data := []byte("must not be uploaded")
	sum := sha256.Sum256(data)
	if _, err := up.Presign(t.Context(), f.editor, media.PresignRequest{
		Ref: ref, Path: "originals/1.png", Type: "image/png", Size: int64(len(data)), SHA256: sum[:],
	}); !errors.Is(err, media.ErrConditionalPutRequired) {
		t.Fatalf("unqualified presign: %v", err)
	}
	body := bytes.NewReader(data)
	if _, err := up.Ingest(t.Context(), f.editor, media.IngestRequest{
		Ref: ref, Path: "originals/1.png", Type: "image/png", Size: int64(len(data)), Body: body,
	}); !errors.Is(err, media.ErrConditionalPutRequired) || body.Len() != len(data) {
		t.Fatalf("unqualified ingest: %v, %d unread bytes", err, body.Len())
	}
	ops := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "refused"}}}
	if _, err := up.Commit(t.Context(), f.editor, ref, uuid.NewString(), ops); !errors.Is(err, media.ErrConditionalPutRequired) {
		t.Fatalf("unqualified commit: %v", err)
	}
	used, reserved, err := limiter.Usage(t.Context(), ref.TenantID, "owner")
	if err != nil || used != 0 || reserved != 0 {
		t.Fatalf("unqualified storage consumed quota: used=%d reserved=%d err=%v", used, reserved, err)
	}
	item, _ := f.reg.Item(ref)
	for obj, err := range env.Store.List(t.Context(), item.Prefix()) {
		t.Fatalf("unqualified storage wrote %s: %v", obj.Key, err)
	}
	if jobs := f.q.take(); len(jobs) != 0 {
		t.Fatalf("unqualified commit queued processing: %+v", jobs)
	}
	payload, err := json.Marshal(media.CommitBody{Ref: media.RefBody{Kind: "gallery", ID: ref.ContentID}, OperationID: uuid.NewString(), Ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	handler := media.UploadHandler(up, media.UploadHandlerOptions{Actor: func(*http.Request) (access.Actor, bool) { return f.editor, true }})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(payload)))
	var reply media.ErrorReply
	if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil || res.Code != http.StatusServiceUnavailable || reply.Code != media.CodeUnavailable {
		t.Fatalf("unqualified HTTP commit: %d %s, %v", res.Code, res.Body.String(), err)
	}
}

func TestManifestRevisionFencesReturningToEarlierContent(t *testing.T) {
	t.Parallel()
	env := s3test.Open(t)
	ms := s3test.Manifests(t, env.Store, miniRegistry(t, env.Tenant), media.ManifestOptions{Journal: env.Journal()})
	ref := contentref.New(env.Tenant, "post", cid(7))
	set := func(value string) (*media.Manifest, error) {
		return ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
			m.Meta = map[string]any{"title": value}
			return nil
		})
	}
	if _, err := set("first"); err != nil {
		t.Fatal(err)
	}
	_, etag, err := ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set("second"); err != nil {
		t.Fatal(err)
	}
	item, _ := ms.Registry().Item(ref)
	rc, _, err := env.Store.Get(t.Context(), item.ManifestKey(), media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set("first"); err != nil {
		t.Fatal(err)
	}
	_, lastETag, err := ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// The delayed edit was prepared against the first state, before a newer
	// writer changed the item and returned it to identical JSON.
	_, err = env.Store.Put(t.Context(), item.ManifestKey(), bytes.NewReader(body), int64(len(body)), media.PutOptions{IfMatch: etag})
	if !errors.Is(err, media.ErrPreconditionFailed) {
		t.Fatalf("late PUT matching an earlier state: %v", err)
	}
	current, _, err := ms.Get(t.Context(), ref)
	if err != nil || current.Meta["title"] != "first" {
		t.Fatalf("late PUT changed the newer content: %+v, %v", current, err)
	}
	if _, err := set("first"); err != nil {
		t.Fatal(err)
	}
	_, unchangedETag, err := ms.Get(t.Context(), ref)
	if err != nil || unchangedETag != lastETag {
		t.Fatalf("semantic no-op changed ETag: %q, %v", unchangedETag, err)
	}
}

// A transient failure during the capability probe (here a 429 on the
// If-None-Match create) must not be recorded as "no conditional PUT".
func TestProbeDoesNotRecordTransientFailures(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	target, _ := url.Parse(env.Config.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var throttled, healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() && r.Method == http.MethodPut && r.Header.Get("If-None-Match") == "*" && strings.Contains(r.URL.Path, "probe-") &&
			throttled.CompareAndSwap(false, true) {
			proxy.ServeHTTP(w, r) // the first create lands
			return
		}
		if !healthy.Load() && r.Method == http.MethodPut && r.Header.Get("If-None-Match") == "*" && throttled.Load() {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`<Error><Code>SlowDown</Code></Error>`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg := env.Config
	cfg.Endpoint, cfg.PublicEndpoint, cfg.Capabilities = srv.URL, "", nil
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Check(ctx, env.Tenant+"/"); err == nil {
		t.Fatalf("a throttled probe succeeded and recorded %+v", store.Capabilities())
	}
	if store.Capabilities() != (media.Capabilities{}) {
		t.Fatalf("a throttled probe recorded %+v", store.Capabilities())
	}
	healthy.Store(true)
	if err := store.Check(ctx, env.Tenant+"/"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.Capabilities(), env.Store.Capabilities(); got != want {
		t.Fatalf("probed %+v after recovery, want %+v", got, want)
	}
}

// Capabilities a host declares (even all false, as on Ceph RGW) are never
// re-probed.
func TestDeclaredCapabilitiesAreNotReprobed(t *testing.T) {
	env := s3test.Open(t)
	store := env.WithCapabilities(t, media.Capabilities{})
	if err := store.Check(context.Background(), env.Tenant+"/"); err != nil {
		t.Fatal(err)
	}
	if store.Capabilities() != (media.Capabilities{}) {
		t.Fatalf("declared capabilities re-probed: %+v", store.Capabilities())
	}
}

// Ceph RGW ignores If-None-Match: * (the create overwrites, so the If-Match
// with the first ETag then answers 412). That backend has no conditional PUT,
// and the probe must say so instead of failing forever.
func TestProbeAcceptsABackendIgnoringIfNoneMatch(t *testing.T) {
	env := s3test.Open(t)
	target, _ := url.Parse(env.Config.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	bucketPrefix := "/" + env.Config.Bucket + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" || !strings.HasPrefix(r.URL.Path, bucketPrefix) {
			proxy.ServeHTTP(w, r)
			return
		}
		// As RGW: If-None-Match is ignored and the PUT overwrites.
		body, _ := io.ReadAll(r.Body)
		obj, err := env.Store.Put(r.Context(), strings.TrimPrefix(r.URL.Path, bucketPrefix), bytes.NewReader(body), int64(len(body)), media.PutOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", obj.ETag)
	}))
	defer srv.Close()
	cfg := env.Config
	cfg.Endpoint, cfg.PublicEndpoint, cfg.Capabilities = srv.URL, "", nil
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background(), env.Tenant+"/"); err != nil {
		t.Fatalf("probe against an If-None-Match-ignoring backend: %v", err)
	}
	if caps := store.Capabilities(); caps.ConditionalPut || caps.ChecksumSHA256 != env.Store.Capabilities().ChecksumSHA256 {
		t.Fatalf("capabilities %+v; want no conditional PUT, checksum as the backend", caps)
	}
}
