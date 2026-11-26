package critical_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// gcClock is a settable clock several goroutines advance.
type gcClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *gcClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *gcClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}
func (c *gcClock) catchUp(at time.Time) {
	c.mu.Lock()
	if at.After(c.at) {
		c.at = at
	}
	c.mu.Unlock()
}

// hookStore runs a hook before a bucket operation, for interleaving a
// collector or a lifecycle call with another writer at an exact step.
type hookStore struct {
	media.Store
	get    func(key string) // after the read returned
	head   func(key string)
	delete func(key string)
	put    func(key string)
	list   func(prefix string)
}

func (s *hookStore) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	rc, obj, err := s.Store.Get(ctx, key, o)
	if s.get != nil {
		s.get(key)
	}
	return rc, obj, err
}

func (s *hookStore) Head(ctx context.Context, key string) (media.Object, error) {
	if s.head != nil {
		s.head(key)
	}
	return s.Store.Head(ctx, key)
}

func (s *hookStore) Delete(ctx context.Context, key string) error {
	if s.delete != nil {
		s.delete(key)
	}
	return s.Store.Delete(ctx, key)
}

func (s *hookStore) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if s.put != nil {
		s.put(key)
	}
	return s.Store.Put(ctx, key, body, size, o)
}

func (s *hookStore) List(ctx context.Context, prefix string) iter.Seq2[media.Object, error] {
	if s.list != nil {
		s.list(prefix)
	}
	return s.Store.List(ctx, prefix)
}

// once runs fn the first time only.
func once(fn func()) func() {
	var o sync.Once
	return func() { o.Do(fn) }
}

// collectAsScheduled runs ref's collection as its GC job does: first at
// at, then whenever the last run asks, until nothing is left.
func collectAsScheduled(t *testing.T, jobs *mediastore.Jobs, clk *gcClock, ref contentref.ContentRef, at time.Time) {
	t.Helper()
	for i := 0; !at.IsZero(); i++ {
		if i == 40 {
			t.Fatalf("collection never finished: still due at %s", at)
		}
		clk.catchUp(at)
		var err error
		if at, err = jobs.Collect(t.Context(), ref); err != nil {
			t.Fatalf("collect run %d: %v", i, err)
		}
	}
}

// TestMediaGCSafety: garbage collection deletes nothing a manifest may name
// again, keeps up with large lists, and leaves no unreferenced object behind;
// deletion and restore leave the gateway fence their manifest records.
func TestMediaGCSafety(t *testing.T) {
	n := newNoteStore(t)
	raw := n.storage.store
	const settle = 10 * time.Minute
	options := func(clk *gcClock) mediastore.ManifestOptions {
		return mediastore.ManifestOptions{Grace: time.Hour, Settle: settle, Now: clk.now}
	}

	t.Run("CK-J09-S60_gc_never_deletes_a_hash_referenced_again_after_its_settle", func(t *testing.T) {
		clk := &gcClock{at: time.Now().UTC()}
		hooked := &hookStore{Store: raw}
		collector, err := mediastore.NewManifests(hooked, n.reg, options(clk))
		must(t, err)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: collector, Now: clk.now})
		must(t, err)
		writer, err := mediastore.NewManifests(raw, n.reg, options(clk))
		must(t, err)
		ref, item := n.item(t)
		bytesOfX := []byte("a page withdrawn while its internal copy was being deleted")
		x := n.put(t, item, layout.AreaInternal, bytesOfX)
		kept := n.put(t, item, layout.AreaPrivate, []byte("kept"))
		_, err = writer.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = []media.File{noteFile("files/x.txt", x, len(bytesOfX)), noteFile("files/kept.txt", kept, 4)}
			return nil
		})
		must(t, err)
		_, err = writer.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = []media.File{noteFile("files/kept.txt", kept, 4)}
			return nil
		})
		must(t, err)
		clk.advance(2 * time.Hour)
		_, err = jobs.Collect(t.Context(), ref)
		must(t, err)
		requireAbsent(t, raw, item, x)
		// Its fence has expired: a withdrawal copies the same bytes back and
		// names them while the next collection is under way, after it read
		// the manifest.
		clk.advance(settle + time.Second)
		manifestKey := item.ManifestKey()
		var rereferenced atomic.Bool
		hooked.get = func(key string) {
			if key != manifestKey || !rereferenced.CompareAndSwap(false, true) {
				return
			}
			n.put(t, item, layout.AreaInternal, bytesOfX)
			_, err := writer.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
				m.Files = append(m.Files, noteFile("files/x-again.txt", x, len(bytesOfX)))
				return nil
			})
			if err != nil {
				t.Errorf("re-reference after the settle period: %v", err)
			}
		}
		_, err = jobs.Collect(t.Context(), ref)
		must(t, err)
		if !rereferenced.Load() {
			t.Fatal("the collection never read the manifest")
		}
		cur, _, err := writer.Get(t.Context(), ref)
		must(t, err)
		if _, ok := cur.Get("files/x-again.txt"); !ok {
			t.Fatal("the re-reference did not land")
		}
		requireObjects(t, raw, item, x, kept)
	})

	t.Run("CK-J09-S61_gc_sends_every_delete_while_its_fence_holds", func(t *testing.T) {
		clk := &gcClock{at: time.Now().UTC()}
		slow := &hookStore{Store: raw}
		manifests, err := mediastore.NewManifests(slow, n.reg, options(clk))
		must(t, err)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: manifests, Now: clk.now})
		must(t, err)
		reader, err := mediastore.NewManifests(raw, n.reg, options(clk))
		must(t, err)
		ref, item := n.item(t)
		const count = 70
		var files []media.File
		var blobs []string
		for i := range count {
			data := []byte(fmt.Sprintf("retired %d", i))
			blob := n.put(t, item, layout.AreaPrivate, data)
			blobs = append(blobs, blob)
			files = append(files, noteFile(fmt.Sprintf("files/%03d.txt", i), blob, len(data)))
		}
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error { m.Files = files; return nil })
		must(t, err)
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error { m.Files = []media.File{}; return nil })
		must(t, err)
		clk.advance(2 * time.Hour)
		// A large folder lists slowly, and the bucket answers each DELETE in
		// ten seconds: a run outlives a single settle period.
		var listed atomic.Bool
		slow.list = func(string) {
			if listed.CompareAndSwap(false, true) {
				clk.advance(settle + time.Minute)
			}
		}
		var mu sync.Mutex
		var late []string
		slow.delete = func(key string) {
			sent := clk.now()
			cur, _, err := reader.GetAny(t.Context(), ref)
			if err != nil {
				t.Errorf("read manifest: %v", err)
				return
			}
			for _, r := range cur.Retired {
				if k, _ := item.Blob(r.Blob); k == key && (r.Deleting == nil || !sent.Before(r.Deleting.Add(settle))) {
					mu.Lock()
					late = append(late, fmt.Sprintf("%s sent at %s, fence %v", r.Blob, sent.Format(time.TimeOnly), r.Deleting))
					mu.Unlock()
				}
			}
			clk.advance(10 * time.Second)
		}
		collectAsScheduled(t, jobs, clk, ref, clk.now())
		if len(late) > 0 {
			t.Fatalf("%d DELETEs were sent after their hash's fence expired, e.g. %s", len(late), late[0])
		}
		requireAbsent(t, raw, item, blobs...)
		cur, _, err := reader.GetAny(t.Context(), ref)
		must(t, err)
		if len(cur.Retired) != 0 {
			t.Fatalf("%d retired entries left after collection finished", len(cur.Retired))
		}
	})

	t.Run("CK-J09-S62_an_object_left_after_the_items_last_write_is_collected", func(t *testing.T) {
		// The item was last written 30 hours ago. A release then copied a
		// file and stopped before its manifest write, and its plan changed:
		// no write will ever name the copy.
		clk := &gcClock{at: time.Now().UTC().Add(-30 * time.Hour)}
		manifests, err := mediastore.NewManifests(raw, n.reg, options(clk))
		must(t, err)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: manifests, Now: clk.now})
		must(t, err)
		ref, item := n.item(t)
		kept := n.put(t, item, layout.AreaPrivate, []byte("kept"))
		m, err := manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = []media.File{noteFile("files/kept.txt", kept, 4)}
			return nil
		})
		must(t, err)
		copied := n.put(t, item, layout.AreaPrivate, []byte("a failed release's copy"))
		collectAsScheduled(t, jobs, clk, ref, jobs.NextCollection(m))
		requireAbsent(t, raw, item, copied)
		requireObjects(t, raw, item, kept)
	})

	fenceStatus := func(t *testing.T, item mediastore.Item, public string) (fence string, served int) {
		t.Helper()
		obj, err := raw.Head(t.Context(), item.PublicPrefix()+layout.FenceName)
		switch {
		case errors.Is(err, mediastore.ErrNotFound):
		case err != nil:
			t.Fatal(err)
		default:
			fence = obj.Metadata[layout.FenceMeta]
			if fence == "" {
				fence = "erased"
			}
		}
		key, err := item.Blob(public)
		must(t, err)
		res, err := http.Get(n.storage.gateway.URL + layout.URLPrefix + key)
		must(t, err)
		res.Body.Close()
		return fence, res.StatusCode
	}
	publicItem := func(t *testing.T) (contentref.ContentRef, mediastore.Item, string) {
		t.Helper()
		ref, item := n.item(t)
		page := n.put(t, item, layout.AreaPublic, []byte("a public page of "+item.Prefix()))
		_, err := mustManifests(t, raw, n.reg).Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			m.Files = []media.File{noteFile("files/page.txt", page, 10)}
			return nil
		})
		must(t, err)
		return ref, item, page
	}

	t.Run("CK-J09-S63_a_deletion_racing_a_restore_leaves_the_fence_its_manifest_records", func(t *testing.T) {
		ref, item, page := publicItem(t)
		deleter, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: mustManifests(t, raw, n.reg)})
		must(t, err)
		must(t, deleter.SetDeleted(t.Context(), ref, true))
		// The restore clears the manifest's deletion; before it looks at the
		// fence, a later deletion runs to the end.
		fenceKey := item.PublicPrefix() + layout.FenceName
		hooked := &hookStore{Store: raw}
		var deletedAgain atomic.Bool
		hooked.head = func(key string) {
			if key == fenceKey && deletedAgain.CompareAndSwap(false, true) {
				if err := deleter.SetDeleted(t.Context(), ref, true); err != nil {
					t.Errorf("racing deletion: %v", err)
				}
			}
		}
		restorer, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: mustManifests(t, hooked, n.reg)})
		must(t, err)
		must(t, restorer.SetDeleted(t.Context(), ref, false))
		if !deletedAgain.Load() {
			t.Fatal("the restore never read the fence")
		}
		m, _, err := mustManifests(t, raw, n.reg).GetAny(t.Context(), ref)
		must(t, err)
		if m.Deleted == nil {
			t.Fatal("the later deletion did not land")
		}
		if fence, status := fenceStatus(t, item, page); fence != layout.FenceDeleted || status != http.StatusNotFound {
			t.Fatalf("a deleted item: fence %q and its public page answers %d, want the deletion fence and 404", fence, status)
		}
	})

	t.Run("CK-J09-S64_a_restore_racing_an_erasure_keeps_its_permanent_fence", func(t *testing.T) {
		ref, item, page := publicItem(t)
		base, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: mustManifests(t, raw, n.reg)})
		must(t, err)
		must(t, base.SetDeleted(t.Context(), ref, true))
		// The restore saw the deletion fence and is about to remove it; the
		// erasure makes the fence permanent first, and its tombstone lands
		// only after the restore read the manifest.
		fenceKey := item.PublicPrefix() + layout.FenceName
		unfencing, fenced, restored := make(chan struct{}), make(chan struct{}), make(chan struct{})
		unfence := once(func() { close(unfencing); <-fenced })
		restoreStore := &hookStore{Store: raw}
		restoreStore.delete = func(key string) {
			if key == fenceKey {
				unfence()
			}
		}
		var tombstoned sync.Once
		eraseStore := &hookStore{Store: raw}
		eraseStore.put = func(key string) {
			if key == item.ManifestKey() {
				tombstoned.Do(func() { close(fenced); <-restored })
			}
		}
		restorer, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: mustManifests(t, restoreStore, n.reg)})
		must(t, err)
		eraser, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: mustManifests(t, eraseStore, n.reg)})
		must(t, err)
		errc := make(chan error, 1)
		go func() {
			defer close(restored)
			errc <- restorer.SetDeleted(t.Context(), ref, false)
		}()
		select {
		case <-unfencing:
		case <-restored:
			t.Fatalf("the restore never removed the deletion fence: %v", <-errc)
		}
		must(t, eraser.Tombstone(t.Context(), ref))
		if err := <-errc; err != nil && !errors.Is(err, mediastore.ErrNotFound) {
			t.Fatalf("restore: %v", err)
		}
		if fence, status := fenceStatus(t, item, page); fence != layout.FenceErased || status != http.StatusNotFound {
			t.Fatalf("an erased item: fence %q and its public page answers %d, want the permanent fence and 404", fence, status)
		}
	})

	t.Run("CK-J09-S66_edits_of_one_manifest_in_one_process_queue_instead_of_conflicting", func(t *testing.T) {
		counting := &countingStore{Store: raw}
		manifests, err := mediastore.NewManifests(counting, n.reg, mediastore.ManifestOptions{})
		must(t, err)
		ref, item := n.item(t)
		const writers = 24
		blobs := make([]string, writers)
		for i := range blobs {
			blobs[i] = n.put(t, item, layout.AreaPrivate, []byte(fmt.Sprintf("page %d", i)))
		}
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				_, err := manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
					path := fmt.Sprintf("files/%02d.txt", i)
					if m.Find(path) >= 0 {
						return mediastore.ErrUnchanged
					}
					m.Files = append(m.Files, noteFile(path, blobs[i], len(fmt.Sprintf("page %d", i))))
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
		if len(got.Files) != writers {
			t.Fatalf("manifest holds %d files, want %d", len(got.Files), writers)
		}
		if refused := counting.refused.Load(); refused != 0 {
			t.Fatalf("one process's edits of one manifest met %d refused conditional writes, want them queued", refused)
		}
	})

	t.Run("CK-J09-S67_gc_drains_the_retired_files_of_a_manifest_at_its_size_limit", func(t *testing.T) {
		clk := &gcClock{at: time.Now().UTC()}
		manifests, err := mediastore.NewManifests(raw, n.reg, options(clk))
		must(t, err)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: manifests, Now: clk.now})
		must(t, err)
		ref, item := n.item(t)
		shared := n.put(t, item, layout.AreaPrivate, []byte("shared"))
		kind, err := n.reg.Kind("note")
		must(t, err)
		// A gallery just under the limit whose last regeneration retired
		// 3,000 files: stamping them all at once would pass the limit.
		const retired = 3000
		target := int64(media.MaxManifestBytes - 40<<10)
		pad := strings.Repeat("p", 3000)
		_, err = manifests.Edit(t.Context(), ref, func(m *mediastore.Manifest) error {
			now := clk.now()
			m.Files, m.Retired = nil, nil
			for i := range retired {
				sum := sha256.Sum256([]byte(fmt.Sprintf("retired %d", i)))
				m.Retired = append(m.Retired, mediastore.Retired{Blob: layout.BlobRef(layout.AreaPrivate, sum[:]), At: now, Due: now.Add(time.Hour)})
			}
			file := func(i int) media.File {
				f := noteFile(fmt.Sprintf("files/%06d.txt", i), shared, 6)
				f.Meta = map[string]any{"pad": pad}
				return f
			}
			size := func() int64 { kind.Normalize(m); return jsonSize(t, m) }
			base := size()
			for i := range 64 {
				m.Files = append(m.Files, file(i))
			}
			per := (size() - base) / 64
			m.Files = m.Files[:0]
			for i := range int((target - base) / per) {
				m.Files = append(m.Files, file(i))
			}
			return nil
		})
		must(t, err)
		if before, _, err := manifests.GetAny(t.Context(), ref); err != nil || len(before.Retired) != retired {
			t.Fatalf("the gallery under its limit was not stored with its retired files: %v", err)
		}
		clk.advance(2 * time.Hour)
		collectAsScheduled(t, jobs, clk, ref, clk.now())
		cur, _, err := manifests.GetAny(t.Context(), ref)
		must(t, err)
		if len(cur.Retired) != 0 {
			t.Fatalf("%d retired entries never drained", len(cur.Retired))
		}
	})
}

// jsonSize is m's stored JSON length.
func jsonSize(t *testing.T, m *mediastore.Manifest) int64 {
	t.Helper()
	body, err := mediastore.EncodeManifest(m)
	must(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(body))
	must(t, err)
	n, err := io.Copy(io.Discard, zr)
	must(t, err)
	return n
}
