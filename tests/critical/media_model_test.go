package critical_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// countingStore counts the conditional PUTs the bucket refused and every
// object write, around the real bucket.
type countingStore struct {
	media.Store
	refused atomic.Int64
	writes  atomic.Int64
	failPut func(key string) error // optional fault, before the write
}

func (s *countingStore) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if s.failPut != nil {
		if err := s.failPut(key); err != nil {
			return media.Object{}, err
		}
	}
	obj, err := s.Store.Put(ctx, key, body, size, o)
	if errors.Is(err, mediastore.ErrPreconditionFailed) {
		s.refused.Add(1)
	} else if err == nil && !strings.HasSuffix(key, layout.ManifestName) {
		s.writes.Add(1)
	}
	return obj, err
}

// PresignEditorGet keeps editor reads of the wrapped bucket working.
func (s *countingStore) PresignEditorGet(ctx context.Context, key string, ttl time.Duration) (media.PresignedRequest, error) {
	return s.Store.(mediastore.EditorReadStore).PresignEditorGet(ctx, key, ttl)
}

func (s *countingStore) Copy(ctx context.Context, src, dst string, o media.CopyOptions) (media.Object, error) {
	obj, err := s.Store.Copy(ctx, src, dst, o)
	if err == nil {
		s.writes.Add(1)
	}
	return obj, err
}

// noteStore is a bucket and a registry of one plain kind, for exercising the
// manifest writer and garbage collection directly.
type noteStore struct {
	storage *mediaWorld
	store   *countingStore
	reg     *mediastore.Registry
	scope   layout.Scope
}

func newNoteStore(t *testing.T) *noteStore {
	t.Helper()
	scope := ck.Scope{TenantID: ck.TenantID(uuid.New()), AppID: ck.AppID(uuid.New())}
	m := newMediaStorage(t, scope)
	ls := layout.Scope{TenantID: scope.TenantID.String(), AppID: scope.AppID.String()}
	reg, err := mediastore.NewRegistry(media.Config{Scope: ls, BaseURL: m.gateway.URL, Kinds: []media.Kind{{Name: "note", KeepOriginals: true,
		Uploads: []media.Upload{{Path: "files/{name}", Types: []string{"text/plain"}, MaxBytes: 1 << 20}}}}}, mediastore.Hooks{})
	must(t, err)
	return &noteStore{storage: m, store: &countingStore{Store: m.store}, reg: reg, scope: ls}
}

func (n *noteStore) item(t *testing.T) (contentref.ContentRef, mediastore.Item) {
	t.Helper()
	ref, err := n.reg.Ref("note", uuid.Must(uuid.NewV7()).String())
	must(t, err)
	item, err := n.reg.Item(ref)
	must(t, err)
	return ref, item
}

// put writes data under its hash in area and returns its blob reference.
func (n *noteStore) put(t *testing.T, item mediastore.Item, area string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	blob := layout.BlobRef(area, sum[:])
	key, err := item.Blob(blob)
	must(t, err)
	_, err = n.storage.store.Put(t.Context(), key, bytes.NewReader(data), int64(len(data)), media.PutOptions{ContentType: "text/plain", ChecksumSHA256: sum[:]})
	must(t, err)
	return blob
}

func noteFile(path, blob string, size int) media.File {
	return media.File{Path: path, Blob: blob, Type: "text/plain", Size: int64(size)}
}

// TestMediaManifestWriter: the bucket is the only record of an item's files,
// rewritten in place with a conditional PUT.
func TestMediaManifestWriter(t *testing.T) {
	n := newNoteStore(t)
	t.Run("CK-J09-S37_store_honours_conditional_put_atomically", func(t *testing.T) {
		// The model's safety rests on the store comparing the precondition
		// with the write itself: of concurrent PUTs carrying the same
		// If-Match (or If-None-Match: *), exactly one lands.
		_, item := n.item(t)
		key := item.ManifestKey()
		for round := 0; round < 3; round++ {
			var current string
			if round > 0 {
				obj, err := n.storage.store.Head(t.Context(), key)
				must(t, err)
				current = obj.ETag
			}
			var landed, refused atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Go(func() {
					body := []byte(fmt.Sprintf("round %d writer %d", round, i))
					o := media.PutOptions{IfMatch: current}
					if current == "" {
						o = media.PutOptions{IfNoneMatch: "*"}
					}
					_, err := n.storage.store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), o)
					switch {
					case err == nil:
						landed.Add(1)
					case errors.Is(err, mediastore.ErrPreconditionFailed):
						refused.Add(1)
					default:
						t.Errorf("conditional put: %v", err)
					}
				})
			}
			wg.Wait()
			if landed.Load() != 1 || refused.Load() != 11 {
				t.Fatalf("round %d: %d writes landed and %d were refused, want 1 and 11", round, landed.Load(), refused.Load())
			}
		}
		caps, err := n.storage.store.Capabilities(t.Context())
		must(t, err)
		if !caps.ConditionalPut || !caps.NeverVersioned {
			t.Fatalf("a fresh bucket must probe writable: %+v", caps)
		}
	})
	t.Run("CK-J09-S36_concurrent_writers_converge_through_412_and_reapply", func(t *testing.T) {
		manifests, err := mediastore.NewManifests(n.store, n.reg, mediastore.ManifestOptions{MaxRetries: 64})
		must(t, err)
		ref, item := n.item(t)
		const writers = 10
		blobs := make([]string, writers)
		for i := range blobs {
			blobs[i] = n.put(t, item, layout.AreaPrivate, []byte(fmt.Sprintf("file %d", i)))
		}
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Go(func() {
				// Each writer's delta adds its own file: re-applied to whatever
				// it reads after a 412, it never undoes another writer's.
				_, err := manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
					path := fmt.Sprintf("files/%02d.txt", i)
					if m.Find(path) >= 0 {
						return mediastore.ErrUnchanged
					}
					m.Files = append(m.Files, noteFile(path, blobs[i], len(fmt.Sprintf("file %d", i))))
					return nil
				})
				if err != nil {
					t.Errorf("writer %d: %v", i, err)
				}
			})
		}
		wg.Wait()
		got, _, err := manifests.Get(t.Context(), ref)
		must(t, err)
		if len(got.Files) != writers || got.Rev != writers {
			t.Fatalf("manifest holds %d files at revision %d, want %d and %d", len(got.Files), got.Rev, writers, writers)
		}
		if n.store.refused.Load() == 0 {
			t.Fatal("concurrent writers never met a 412: the race was not exercised")
		}
		// A retry of a landed write (same op) changes nothing.
		before := got.Rev
		op := uuid.Must(uuid.NewV7()).String()
		for range 2 {
			_, err = manifests.EditWith(t.Context(), ref, mediastore.EditOptions{Op: op}, func(m *mediastore.Manifest) error {
				if m.Meta == nil {
					m.Meta = map[string]any{}
				}
				m.Meta["title"] = "once"
				return nil
			})
			must(t, err)
		}
		got, _, err = manifests.Get(t.Context(), ref)
		must(t, err)
		if got.Rev != before+1 {
			t.Fatalf("a repeated op wrote again: revision %d, want %d", got.Rev, before+1)
		}
		// A manifest never names bytes the bucket does not hold.
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = append(m.Files, noteFile("files/missing.txt", layout.BlobRef(layout.AreaPrivate, make([]byte, 32)), 1))
			return nil
		})
		var missing *mediastore.MissingObjectError
		if !errors.As(err, &missing) {
			t.Fatalf("referencing absent bytes: %v, want MissingObjectError", err)
		}
	})
	t.Run("CK-J09-S38_a_bucket_that_had_versioning_refuses_media_writes", func(t *testing.T) {
		versioned := newMediaStorage(t, ck.Scope{TenantID: ck.TenantID(uuid.New()), AppID: ck.AppID(uuid.New())})
		enableVersioning(t, versioned)
		if _, err := versioned.store.Capabilities(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := mediastore.RequireWritable(t.Context(), versioned.store); !errors.Is(err, mediastore.ErrBucketVersioned) {
			t.Fatalf("a versioned bucket: %v, want ErrBucketVersioned", err)
		}
	})
}

// TestMediaGarbageCollection: GC deletes only what the manifest retired and
// nothing references any more.
func TestMediaGarbageCollection(t *testing.T) {
	n := newNoteStore(t)
	t.Run("CK-J09-S39_gc_deletes_only_retired_unreferenced_files", func(t *testing.T) {
		now := time.Now().UTC()
		var clock atomic.Pointer[time.Time]
		clock.Store(&now)
		tick := func() time.Time { return *clock.Load() }
		manifests, err := mediastore.NewManifests(n.storage.store, n.reg, mediastore.ManifestOptions{Grace: time.Hour, Settle: 10 * time.Minute, Now: tick})
		must(t, err)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: manifests, Now: tick})
		must(t, err)
		ref, item := n.item(t)
		a := n.put(t, item, layout.AreaPrivate, []byte("a"))
		b := n.put(t, item, layout.AreaPrivate, []byte("b"))
		c := n.put(t, item, layout.AreaPrivate, []byte("c"))
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = []media.File{noteFile("files/a.txt", a, 1), noteFile("files/b.txt", b, 1), noteFile("files/c.txt", c, 1)}
			return nil
		})
		must(t, err)
		// a is replaced by d and retired; c is removed and then re-referenced
		// (the same bytes again) before it is due: only a may go.
		d := n.put(t, item, layout.AreaPrivate, []byte("d"))
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = []media.File{noteFile("files/a.txt", d, 1), noteFile("files/b.txt", b, 1)}
			return nil
		})
		must(t, err)
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = append(m.Files, noteFile("files/c2.txt", c, 1))
			return nil
		})
		must(t, err)
		cur, _, err := manifests.Get(t.Context(), ref)
		must(t, err)
		if len(cur.Retired) != 1 || cur.Retired[0].Blob != a {
			t.Fatalf("retired %+v, want only %s", cur.Retired, a)
		}
		// Not yet due: nothing is deleted.
		_, err = jobs.Collect(t.Context(), ref)
		must(t, err)
		requireObjects(t, n.storage.store, item, a, b, c, d)
		later := now.Add(2 * time.Hour)
		clock.Store(&later)
		next, err := jobs.Collect(t.Context(), ref)
		must(t, err)
		requireObjects(t, n.storage.store, item, b, c, d)
		requireAbsent(t, n.storage.store, item, a)
		if next.IsZero() {
			t.Fatal("a deleted entry must stay until its delete has settled")
		}
		// While a's DELETE may still land, its hash is not referenced again.
		n.put(t, item, layout.AreaPrivate, []byte("a"))
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = append(m.Files, noteFile("files/a-again.txt", a, 1))
			return nil
		})
		var settling *mediastore.SettlingError
		if !errors.As(err, &settling) {
			t.Fatalf("re-referencing a deleting hash: %v, want SettlingError", err)
		}
		settled := later.Add(20 * time.Minute)
		clock.Store(&settled)
		_, err = jobs.Collect(t.Context(), ref)
		must(t, err)
		cur, _, err = manifests.Get(t.Context(), ref)
		must(t, err)
		if len(cur.Retired) != 0 {
			t.Fatalf("settled entries kept: %+v", cur.Retired)
		}
		// An object nobody names, written by a crashed writer, is an orphan
		// the pass after the item's last write removes once a day old.
		orphan := n.put(t, item, layout.AreaInternal, []byte("orphan"))
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Meta = map[string]any{"title": "touched"}
			return nil
		})
		must(t, err)
		_, err = jobs.Collect(t.Context(), ref)
		must(t, err)
		requireObjects(t, n.storage.store, item, orphan) // younger than a day
		dayLater := settled.Add(25 * time.Hour)
		clock.Store(&dayLater)
		_, err = jobs.Collect(t.Context(), ref)
		must(t, err)
		requireAbsent(t, n.storage.store, item, orphan)
		requireObjects(t, n.storage.store, item, b, c, d)
	})
}

// enableVersioning turns versioning on for a test bucket, as an operator
// mistake would.
func enableVersioning(t *testing.T, m *mediaWorld) {
	t.Helper()
	_, err := m.management.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{Bucket: &m.bucket,
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	must(t, err)
	// Runs before the bucket's own cleanup: every version and delete marker
	// goes, so the bucket can be deleted.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := m.management.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: &m.bucket})
		if err != nil {
			t.Errorf("list versions: %v", err)
			return
		}
		for _, v := range out.Versions {
			_, _ = m.management.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: &m.bucket, Key: v.Key, VersionId: v.VersionId})
		}
		for _, d := range out.DeleteMarkers {
			_, _ = m.management.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: &m.bucket, Key: d.Key, VersionId: d.VersionId})
		}
	})
}

func requireObjects(t *testing.T, store media.Store, item mediastore.Item, blobs ...string) {
	t.Helper()
	for _, blob := range blobs {
		key, _ := item.Blob(blob)
		if _, err := store.Head(t.Context(), key); err != nil {
			t.Fatalf("%s: %v", blob, err)
		}
	}
}

func requireAbsent(t *testing.T, store media.Store, item mediastore.Item, blobs ...string) {
	t.Helper()
	for _, blob := range blobs {
		key, _ := item.Blob(blob)
		if _, err := store.Head(t.Context(), key); !errors.Is(err, mediastore.ErrNotFound) {
			t.Fatalf("%s still present: %v", blob, err)
		}
	}
}

// TestMediaGatewayAreas: the gateway serves public/ to anyone, private/ to
// its own item's token, and never internal/, tmp/ or a manifest.
func TestMediaGatewayAreas(t *testing.T) {
	n := newNoteStore(t)
	_, first := n.item(t)
	_, second := n.item(t)
	served := map[string]string{}
	for _, it := range []mediastore.Item{first, second} {
		for _, area := range []string{layout.AreaPublic, layout.AreaPrivate, layout.AreaInternal} {
			blob := n.put(t, it, area, []byte(area+" of "+it.Prefix()))
			key, _ := it.Blob(blob)
			served[it.Prefix()+area] = key
		}
	}
	ring, err := token.NewRing(n.storage.key, nil)
	must(t, err)
	firstToken := ring.Sign(token.ItemScope(first.Prefix()), time.Now().Add(time.Hour))
	get := func(key, cookie string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, n.storage.gateway.URL+layout.URLPrefix+key, nil)
		must(t, err)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: token.CookieName, Value: cookie})
		}
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	t.Run("CK-J09-S40_a_cookie_reaches_only_its_items_private_area", func(t *testing.T) {
		for _, c := range []struct {
			key, cookie string
			want        int
		}{
			{served[first.Prefix()+layout.AreaPublic], "", 200},
			{served[second.Prefix()+layout.AreaPublic], "", 200},
			{served[first.Prefix()+layout.AreaPrivate], "", 404},
			{served[first.Prefix()+layout.AreaPrivate], firstToken, 200},
			{served[second.Prefix()+layout.AreaPrivate], firstToken, 404},
		} {
			if got := get(c.key, c.cookie); got != c.want {
				t.Fatalf("GET %s (token %t): %d, want %d", c.key, c.cookie != "", got, c.want)
			}
		}
	})
	t.Run("CK-J09-S41_unreleased_files_are_unreachable", func(t *testing.T) {
		staged := first.TempPrefix() + "u-" + uuid.NewString()
		_, err := n.storage.store.Put(t.Context(), staged, strings.NewReader("staged"), 6, media.PutOptions{})
		must(t, err)
		_, err = n.storage.store.Put(t.Context(), first.ManifestKey(), strings.NewReader("{}"), 2, media.PutOptions{})
		must(t, err)
		for _, key := range []string{served[first.Prefix()+layout.AreaInternal], staged, first.ManifestKey()} {
			for _, cookie := range []string{"", firstToken} {
				if got := get(key, cookie); got != 404 {
					t.Fatalf("GET %s (token %t): %d, want 404", key, cookie != "", got)
				}
			}
		}
	})
}

// releasedGallery is a published gallery of pages with its manifest store.
type releasedGallery struct {
	world     *world
	app       *application
	storage   *mediaWorld
	manifests *mediastore.Manifests
	reg       *mediastore.Registry
	ref       media.RefBody
	cref      contentref.ContentRef
}

func newReleasedGallery(t *testing.T, store func(media.Store) media.Store, pages ...[]byte) *releasedGallery {
	t.Helper()
	w := newWorld(t)
	a := w.apps[0]
	m := attachMediaWith(t, w, a, store, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	config, err := a.cfg.MediaKinds()
	must(t, err)
	reg, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	manifests, err := mediastore.NewManifests(m.store, reg, mediastore.ManifestOptions{})
	must(t, err)
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Released by a move")
	var ops []media.Op
	for i, page := range pages {
		ops = append(ops, stageFile(t, admin, ref, fmt.Sprintf("pages/%03d.png", i+1), page))
	}
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: ops})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	cref, err := reg.VersionRef("gallery", ref.ID, ref.Version)
	must(t, err)
	return &releasedGallery{world: w, app: a, storage: m, manifests: manifests, reg: reg, ref: ref, cref: cref}
}

func (g *releasedGallery) publish(t *testing.T) {
	t.Helper()
	_, err := g.app.engine.Client().PostContentPublish(t.Context(), g.ref.ID, catalog.PublishInput{})
	must(t, err)
}

// awaitReleased waits until every reader rendition is in a served area.
func (g *releasedGallery) awaitReleased(t *testing.T) *mediastore.Manifest {
	t.Helper()
	var m *mediastore.Manifest
	waitForWithin(t, 45*time.Second, func() bool {
		cur, _, err := g.manifests.Get(t.Context(), g.cref)
		if err != nil {
			return false
		}
		renditions := 0
		for _, f := range cur.Files {
			if f.Preset == "reader" {
				if !layout.Served(layout.BlobArea(f.Blob)) {
					return false
				}
				renditions++
			}
		}
		m = cur
		return renditions > 0
	})
	return m
}

// TestMediaReleaseByMove: a release copies a version's files into a served
// area, switches the manifest, and retires the internal copies.
func TestMediaReleaseByMove(t *testing.T) {
	t.Run("CK-J09-S42_a_release_leaves_no_internal_copy", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 11), chapterImage(t, 23))
		item, err := g.reg.Item(g.cref)
		must(t, err)
		before, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		for _, f := range before.Files {
			if f.Blob != "" && layout.BlobArea(f.Blob) == layout.AreaPrivate {
				t.Fatalf("an unreleased draft serves %s", f.Path)
			}
		}
		g.publish(t)
		m := g.awaitReleased(t)
		var leftovers []string
		for _, f := range m.Files {
			if f.Preset == "reader" {
				leftovers = append(leftovers, layout.MoveRef(f.Blob, layout.AreaInternal))
			}
		}
		for _, blob := range leftovers {
			r, ok := m.Retiring(blob)
			if !ok || r.Due.Sub(r.At) > 2*time.Hour {
				t.Fatalf("release leftover %s not retired for an hour: %+v", blob, r)
			}
		}
		// Collect once the leftovers are due: no internal copy remains.
		due := time.Now().Add(2 * time.Hour)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: g.manifests, Now: func() time.Time { return due }})
		must(t, err)
		_, err = jobs.Collect(t.Context(), g.cref)
		must(t, err)
		requireAbsent(t, g.storage.store, item, leftovers...)
		var served []string
		for _, f := range m.Files {
			if f.Preset == "reader" {
				served = append(served, f.Blob)
			}
		}
		requireObjects(t, g.storage.store, item, served...)
	})
	t.Run("CK-J09-S43_a_crash_at_each_release_step_recovers", func(t *testing.T) {
		// The engine's bucket refuses this version's manifest writes while
		// the fault is on: the release copies, then cannot switch, as if it
		// crashed between its copy and its manifest write.
		var fault atomic.Bool
		var version atomic.Value
		var hits atomic.Int64
		g := newReleasedGallery(t, func(store media.Store) media.Store {
			return &countingStore{Store: store, failPut: func(key string) error {
				if v, _ := version.Load().(string); fault.Load() && v != "" && strings.Contains(key, v) && strings.HasSuffix(key, layout.ManifestName) {
					hits.Add(1)
					return errors.New("injected: the release stopped before its manifest write")
				}
				return nil
			}}
		}, chapterImage(t, 31))
		version.Store(g.ref.Version)
		fault.Store(true)
		g.publish(t)
		waitForWithin(t, 30*time.Second, func() bool { return hits.Load() > 0 })
		// Copies landed in the served area; the manifest still names the
		// internal files, so readers see the old, complete state.
		mid, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		for _, f := range mid.Files {
			if f.Preset == "reader" && layout.BlobArea(f.Blob) != layout.AreaInternal {
				t.Fatalf("a failed switch moved %s", f.Path)
			}
		}
		fault.Store(false)
		m := g.awaitReleased(t)
		q := pgx.Identifier{g.world.schema, "content_versions"}.Sanitize()
		var released bool
		waitForWithin(t, 30*time.Second, func() bool {
			must(t, g.world.pool.QueryRow(t.Context(), `SELECT released FROM `+q+` WHERE id=$1`, g.ref.Version).Scan(&released))
			return released
		})
		// A crash after the manifest switched, before the catalog recorded
		// it: the next run finds nothing to move and completes the catalog.
		_, err = g.world.pool.Exec(t.Context(), `UPDATE `+q+` SET released=false WHERE id=$1`, g.ref.Version)
		must(t, err)
		g.publish(t)
		waitForWithin(t, 30*time.Second, func() bool {
			must(t, g.world.pool.QueryRow(t.Context(), `SELECT released FROM `+q+` WHERE id=$1`, g.ref.Version).Scan(&released))
			return released
		})
		after, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		if after.Rev != m.Rev {
			t.Fatalf("a completed release re-ran its move: revision %d -> %d", m.Rev, after.Rev)
		}
	})
}

// TestMediaRederive: an output whose fingerprint matches is never produced
// again: a derivative pass over an unchanged item writes nothing.
func TestMediaRederive(t *testing.T) {
	t.Run("CK-J09-S44_a_rederive_writes_nothing", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 7))
		jobs, err := river.NewClient(riverpgxv5.New(g.world.pool), &river.Config{Schema: g.app.cfg.Media.WorkerSchema})
		must(t, err)
		settle := func() {
			t.Helper()
			waitForWithin(t, 45*time.Second, func() bool {
				pending, err := jobs.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.IngestArgs{}).Kind(), (workqueue.DerivativeArgs{}).Kind()).States(
					rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled, rivertype.JobStatePending).First(1))
				must(t, err)
				return len(pending.Jobs) == 0
			})
		}
		settle()
		before, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		objects := bucketKeys(t, g.storage)
		queue := mustQueue(t, g.world.pool, g.reg, g.app.cfg.Media.WorkerSchema)
		must(t, queue.Enqueue(t.Context(), mediastore.ProcessJob{Ref: g.cref}))
		settle()
		after, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		if after.Rev != before.Rev {
			t.Fatalf("a re-derive rewrote the manifest: revision %d -> %d", before.Rev, after.Rev)
		}
		if got := bucketKeys(t, g.storage); !equalKeys(objects, got) {
			t.Fatalf("a re-derive wrote objects:\nbefore %v\nafter  %v", objects, got)
		}
	})
}

func bucketKeys(t *testing.T, m *mediaWorld) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	for obj, err := range m.store.List(t.Context(), "") {
		must(t, err)
		if !strings.HasSuffix(obj.Key, layout.ManifestName) {
			out[obj.Key] = obj.LastModified
		}
	}
	return out
}

func equalKeys(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !w.Equal(v) {
			return false
		}
	}
	return true
}

// TestMediaZipDownload: a version's ZIP is streamed from its originals and
// is the same bytes on every request.
func TestMediaZipDownload(t *testing.T) {
	t.Run("CK-J09-S45_the_zip_is_byte_identical_across_requests", func(t *testing.T) {
		pages := [][]byte{chapterImage(t, 3), chapterImage(t, 5), chapterImage(t, 9)}
		g := newReleasedGallery(t, nil, pages...)
		g.publish(t)
		g.awaitReleased(t)
		admin := g.app.engine.Client()
		fetch := func(rangeHeader string) (*http.Response, []byte) {
			t.Helper()
			reply, err := admin.GetContentVersionDownload(t.Context(), g.ref.ID, g.ref.Version)
			must(t, err)
			var grant versions.FileDownload
			must(t, json.Unmarshal(reply.Body, &grant))
			base, err := url.Parse(g.app.server.URL + "/content/v1/contents/" + g.ref.ID + "/versions/" + g.ref.Version + "/download")
			must(t, err)
			target, err := base.Parse(grant.URL)
			must(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.String(), nil)
			must(t, err)
			if rangeHeader != "" {
				req.Header.Set("Range", rangeHeader)
			}
			res, err := http.DefaultClient.Do(req)
			must(t, err)
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			must(t, err)
			return res, body
		}
		first, a := fetch("")
		second, b := fetch("")
		if first.StatusCode != 200 || !bytes.Equal(a, b) {
			t.Fatalf("two downloads differ: %d, %d and %d bytes", first.StatusCode, len(a), len(b))
		}
		if first.Header.Get("Content-Length") != fmt.Sprint(len(a)) || first.Header.Get("ETag") == "" || first.Header.Get("ETag") != second.Header.Get("ETag") {
			t.Fatalf("ZIP headers: length %q for %d bytes, ETags %q %q", first.Header.Get("Content-Length"), len(a), first.Header.Get("ETag"), second.Header.Get("ETag"))
		}
		zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
		must(t, err)
		if len(zr.File) != len(pages) {
			t.Fatalf("ZIP holds %d entries, want %d", len(zr.File), len(pages))
		}
		for i, f := range zr.File {
			rc, err := f.Open()
			must(t, err)
			got, err := io.ReadAll(rc)
			rc.Close()
			must(t, err)
			if !bytes.Equal(got, pages[i]) {
				t.Fatalf("entry %s is not the uploaded original", f.Name)
			}
		}
		partial, slice := fetch("bytes=10-99")
		if partial.StatusCode != http.StatusPartialContent || !bytes.Equal(slice, a[10:100]) {
			t.Fatalf("range resume: %d with %d bytes", partial.StatusCode, len(slice))
		}
	})
}

// TestMediaCacheRebuild: the database keeps only a cache of the bucket's
// manifests, which a rebuild restores.
func TestMediaCacheRebuild(t *testing.T) {
	t.Run("CK-J09-S46_a_dropped_database_cache_rebuilt_from_the_bucket_serves_the_same_lists", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 13), chapterImage(t, 17))
		g.publish(t)
		g.awaitReleased(t)
		reader := g.app.engine.Client()
		snapshot := func() (string, catalog.Version) {
			t.Helper()
			var cached string
			must(t, g.world.pool.QueryRow(t.Context(), `SELECT rev::text||state||images::text FROM `+pgx.Identifier{g.world.schema, "content_media_items"}.Sanitize()+
				` WHERE content_version_id=$1`, g.ref.Version).Scan(&cached))
			v, err := reader.GetContentVersion(t.Context(), g.ref.ID, g.ref.Version, ck.GetContentVersionQuery{})
			must(t, err)
			return cached, v
		}
		cached, before := snapshot()
		q := func(table string) string { return pgx.Identifier{g.world.schema, table}.Sanitize() }
		_, err := g.world.pool.Exec(t.Context(), `DELETE FROM `+q("content_media_items"))
		must(t, err)
		_, err = g.world.pool.Exec(t.Context(), `UPDATE `+q("content_versions")+` SET pages=NULL,duration_ms=NULL,width=NULL,height=NULL WHERE id=$1`, g.ref.Version)
		must(t, err)
		config, err := g.app.cfg.MediaKinds()
		must(t, err)
		registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
		must(t, err)
		n, err := worker.Rebuild(t.Context(), worker.Config{Pool: g.world.pool, Schema: g.app.cfg.Media.WorkerSchema, ContentSchema: g.world.schema,
			HostSchema: g.world.schema + "_river", Store: g.storage.store, Kinds: registry})
		must(t, err)
		if n == 0 {
			t.Fatal("rebuild recorded no item")
		}
		cachedAfter, after := snapshot()
		pages := func(v catalog.Version) string {
			if v.Pages == nil {
				return "none"
			}
			return fmt.Sprint(*v.Pages)
		}
		if cachedAfter != cached || pages(before) != pages(after) || before.Live != after.Live {
			t.Fatalf("rebuilt cache differs:\n%s %s %v\n%s %s %v", cached, pages(before), before.Live, cachedAfter, pages(after), after.Live)
		}
	})
}

// mustManifests reads and writes the bucket's manifests directly, as the
// engine and the worker do.
func mustManifests(t *testing.T, store media.Store, reg *mediastore.Registry) *mediastore.Manifests {
	t.Helper()
	m, err := mediastore.NewManifests(store, reg, mediastore.ManifestOptions{})
	must(t, err)
	return m
}

// privateFileScope is the token scope of the item holding a private key:
// there are no per-file tokens.
func privateFileScope(key string) string {
	k, _ := layout.Parse(key)
	return token.ItemScope(k.Folder())
}

// mustQueue is the worker's processing queue.
func mustQueue(t *testing.T, pool *pgxpool.Pool, reg *mediastore.Registry, schema string) *workqueue.Queue {
	t.Helper()
	q, err := workqueue.New(pool, reg, schema)
	must(t, err)
	return q
}
